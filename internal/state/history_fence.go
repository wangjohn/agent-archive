package state

import (
	"encoding/json"

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
	if err := archive.CheckHistoryMutation(cached, metadata); err != nil {
		return err
	}
	previous, _, _ := published.LastPublished()
	return archive.CheckHistoryMutation(previous, archive.Metadata{})
}
