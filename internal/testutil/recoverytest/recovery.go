// Package recoverytest exhausts the actual scheduled recovery engine for fixtures.
// Production code must retain its bounded scheduling behavior.
package recoverytest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Store is the recovery surface exercised by synthetic fixtures.
type Store interface {
	Home() string
	MarkSessionIndexRecoveryNeeded() error
	RecoverSessionIndexScheduled(context.Context, time.Duration) (bool, error)
}

// Exhaust completes actual scheduled slices within ctx. Fresh requests another
// census after an earlier completion; pending requests retain their generation.
func Exhaust(ctx context.Context, store Store, allowance time.Duration, fresh bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fresh {
		var marker struct {
			Version  int  `json:"version"`
			Complete bool `json:"complete"`
		}
		data, err := os.ReadFile(filepath.Join(store.Home(), "session-index.json"))
		if err == nil && json.Unmarshal(data, &marker) == nil && marker.Version == 1 && marker.Complete {
			if err := store.MarkSessionIndexRecoveryNeeded(); err != nil {
				return err
			}
		}
	}
	for {
		complete, err := store.RecoverSessionIndexScheduled(ctx, allowance)
		if err != nil || complete {
			return err
		}
	}
}
