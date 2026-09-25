package cli

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

func installedVersionSupport(discovery applicationDiscovery, verifiedVersions []string) string {
	state, _ := installedVersionSupportDetail(discovery, verifiedVersions)
	return state
}

// pendingSessions counts every accepted session state.Outstanding reports
// as pending, a session waiting for its transcript included: status reports
// it as pending, because from the user's side it is.
func pendingSessions(home string, cfg config.Config) (int, error) {
	blocking, waiting, err := pendingSessionCounts(home, cfg)
	return blocking + waiting, err
}

// Verify only new publications or evidence invalidated by configuration changes.
// Status reads this local result; it never downloads conversation content.
//
// A failed read-back is recorded with a retry time and never surfaces as a
// pass error: the publication itself succeeded, so `sync` still exits 0 and
// Status.LastError stays free for genuine collection failures. Status reports
// the pending or failed verification from the record instead.
//
// Local state that cannot be read fails only its own session's read-back:
// an unreadable registration is skipped (the collector pass has already
// reported it), and a session whose published state or verification record
// cannot be read is left out and returned as an error once every other
// session has been handled.
func verifyPublications(home string, cfg config.Config, env Env, store *state.Store, remote storage.ObjectStore) (verificationSummary, error) {
	return verifyPublicationsWithin(context.Background(), home, cfg, env, store, remote)
}
