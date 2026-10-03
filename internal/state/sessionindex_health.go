package state

import (
	"path/filepath"
)

// SessionIndexRecoveryStatus exposes bounded content-free scheduling evidence.
// An error means unknown evidence; callers must not infer completed coverage.
type SessionIndexRecoveryStatus struct {
	Complete bool   `json:"complete"`
	Pending  bool   `json:"pending"`
	Phase    string `json:"phase"`
}

// SessionIndexRecoveryStatus reads the recovery marker, cursor and membership fence. It
// never enumerates registrations or reads a transcript, Git metadata or storage.
// It reads at most 2560 bytes of state, excluding one bounded oversize byte per file.
func (s *Store) SessionIndexRecoveryStatus() (SessionIndexRecoveryStatus, error) {
	var marker sessionIndexMarker
	if err := readRecoveryJSONLimit(filepath.Join(s.home, sessionIndexMarkerFile), &marker, 512); err != nil {
		return SessionIndexRecoveryStatus{Phase: "unknown"}, err
	}
	if marker.Version != 1 {
		return SessionIndexRecoveryStatus{Phase: "unknown"}, ErrSessionIndexRecoveryRequired
	}
	if marker.Complete {
		if marker.MembershipFenced {
			if err := s.validateRecoveryHealthFence(""); err != nil {
				return SessionIndexRecoveryStatus{Phase: "unknown"}, err
			}
		}
		return SessionIndexRecoveryStatus{Complete: true, Phase: "complete"}, nil
	}
	return s.pendingRecoveryHealth(marker)
}

func (s *Store) validateRecoveryHealthFence(expected string) error {
	var revision sessionMembershipRevision
	if err := readRecoveryJSONLimit(filepath.Join(s.home, sessionMembershipFile), &revision, 512); err != nil {
		return err
	}
	if revision.Version != 1 || !safeFileComponent(revision.Revision) || (expected != "" && revision.Revision != expected) {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

func (s *Store) pendingRecoveryHealth(marker sessionIndexMarker) (SessionIndexRecoveryStatus, error) {
	unknown := SessionIndexRecoveryStatus{Pending: true, Phase: "unknown"}
	var cursor sessionRecoveryCursor
	if err := readRecoveryJSONLimit(filepath.Join(s.home, sessionRecoveryCursorFile), &cursor, 1536); err != nil {
		return unknown, err
	}
	if cursor.Version != 1 || cursor.Generation != marker.Generation || !cursor.validChecksum() || cursor.Offset < 0 || cursor.Phase < 0 || cursor.Phase > 2 {
		return unknown, ErrSessionIndexRecoveryRequired
	}
	if marker.MembershipFenced || cursor.Revision != "" {
		if err := s.validateRecoveryHealthFence(cursor.Revision); err != nil {
			return unknown, err
		}
	}
	return SessionIndexRecoveryStatus{Pending: true, Phase: []string{"registrations", "candidates", "requested-misses"}[cursor.Phase]}, nil
}
