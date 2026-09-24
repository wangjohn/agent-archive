package collector

import (
	"errors"
	"fmt"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// evidenceOnly decodes a stored bundle's supplemental evidence and nothing
// else, so its native records, which can be tens of megabytes, are skipped
// rather than decoded.
type evidenceOnly struct {
	Bundle struct {
		SupplementalEvidence []archive.SupplementalEvidence `json:"supplemental_evidence"`
	} `json:"bundle"`
	// DeferredHookEvidence is the published state's hook evidence held by a
	// recoverable block (see publishedState); the pending file has none.
	DeferredHookEvidence []archive.SupplementalEvidence `json:"deferred_hook_evidence"`
}

// StoredEvidence returns the supplemental evidence of a session's pending
// publication and of its published state (the bundle LoadPending and
// LoadPublished return, and hook evidence a recoverable block deferred),
// pending first, without decoding either bundle's
// records or the pending publication's source bytes. A session with neither
// returns nothing. It is read-only.
func (s *LocalStore) StoredEvidence(archiveSessionID string) ([]archive.SupplementalEvidence, error) {
	if !safeFileComponent(archiveSessionID) {
		return nil, errors.New("archive session ID is not a safe file name component")
	}
	var out []archive.SupplementalEvidence
	for _, file := range []struct{ path, what string }{
		{s.pendingPath(archiveSessionID), "pending publication"},
		{s.publishedPath(archiveSessionID), "published state"},
	} {
		var state evidenceOnly
		err := local.Read(file.path, &state)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s %q: %w", file.what, archiveSessionID, err)
		}
		out = append(out, state.Bundle.SupplementalEvidence...)
		out = append(out, state.DeferredHookEvidence...)
	}
	return out, nil
}
