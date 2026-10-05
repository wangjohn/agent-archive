package state

import (
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

// SessionIndexAbsent reports a miss established by a completed registration
// census. Missing derived indexes alone never authorize discovery allocation.
// The caller holds hooks.lock so a recovery request cannot invalidate this
// evidence between the check and registration.
func (s *Store) SessionIndexAbsent(key agentmeta.SessionKey) (bool, error) {
	if err := key.Validate(); err != nil {
		return false, err
	}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil {
		return false, err
	}
	if !found || !entry.Absent {
		return false, nil
	}
	var marker sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
		return false, ErrSessionIndexRecoveryRequired
	}
	if !recoveryMarkerVersion(marker.Version) || !marker.Complete {
		return false, ErrSessionIndexRecoveryRequired
	}
	if marker.MembershipFenced {
		if _, err := s.sessionMembershipRevision(); err != nil {
			return false, err
		}
	}
	return true, nil
}
