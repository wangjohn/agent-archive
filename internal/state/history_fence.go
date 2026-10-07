package state

import (
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// CheckHistoryRecovery fences explicit recovery until retained revision lifecycle is enabled.
// It is called only for a user-requested mutation, never from unchanged scan admission.
func (s *Store) CheckHistoryRecovery(id string) error {
	published, err := s.LoadPublishedState(id)
	if err != nil {
		return err
	}
	cached, _, _, _ := published.Cached()
	var metadata archive.Metadata
	if raw := published.Metadata(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return err
		}
	}
	previous, _, _ := published.LastPublished()
	if cached.History == nil && previous.History == nil && metadata.History == nil {
		return archive.CheckHistoryMutation(cached, metadata)
	}
	if err := cached.ValidateHistory(); err != nil {
		return errors.Join(archive.ErrHistoryMutationPending, err)
	}
	if err := previous.ValidateHistory(); err != nil {
		return errors.Join(archive.ErrHistoryMutationPending, err)
	}
	if _, err := metadata.SourceReferences(); err != nil {
		return errors.Join(archive.ErrHistoryMutationPending, err)
	}
	reg, found, err := s.LoadRegistration(id)
	if err != nil || !found || metadata.SessionID != id || metadata.NativeSessionID != reg.NativeSessionID || metadata.ProjectID != reg.ProjectID || metadata.Harness.Name != reg.Harness.Name {
		return errors.Join(archive.ErrHistoryMutationPending, errors.New("complete recovery history authority required"), err)
	}
	return nil
}
