package state

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// LegacyCodexCompositeReservation observes one exact legacy derived identity.
// It never grants ownership, certifies absence, allocates an ID or enumerates
// indexes. A positive result requires a validated matching index and no admitted
// registration. Missing/corrupt local evidence remains unproven.
func (s *Store) LegacyCodexCompositeReservation(parent, child string) (string, bool, error) {
	if parent == "" || child == "" || codexmeta.RolloutID(parent+".jsonl") != parent || codexmeta.RolloutID(child+".jsonl") != child {
		return "", false, errors.New("invalid native relationship identity")
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: parent + ":subagent:" + child}
	entry, found, err := s.readQualifiedIndex(key)
	if err != nil || !found || entry.Absent {
		return "", false, err
	}
	if _, registered, err := s.LoadRegistration(entry.ArchiveSessionID); err != nil || registered {
		return "", false, err
	}
	return entry.ArchiveSessionID, true, nil
}
