package capture

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
)

// PrepareIdentityIndexes repairs old identities independently of discovery or
// storage. Call it outside hooks.lock; it advances one bounded migration batch.
func PrepareIdentityIndexes(home string, limit int) (bool, error) {
	return state.OpenReadOnly(home).ReconcileIdentityIndexes(limit, func() error {
		if setupjournal.TransactionPending(home) {
			return errors.New("setup pending during identity migration")
		}
		return config.ProtectIdentityWriter(home)
	})
}
