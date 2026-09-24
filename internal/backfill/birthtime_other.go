//go:build !darwin

package backfill

import (
	"os"
	"time"
)

// fileBirthTime falls back to the modification time where the standard
// library exposes no birth time. Backfill targets macOS; this keeps the
// package building and testable elsewhere.
func fileBirthTime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
