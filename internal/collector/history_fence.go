package collector

import (
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func (s *sessionScan) checkRetainedHistory() error {
	b, _, _, _ := s.published.Cached()
	var m archive.Metadata
	if raw := s.published.Metadata(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
	}
	if b.History == nil && m.History == nil {
		return archive.CheckHistoryMutation(b, m)
	}
	if err := b.ValidateHistory(); err != nil {
		return errors.Join(archive.ErrHistoryMutationPending, err)
	}
	if err := s.validateAuthorityIdentity(m); err != nil {
		return errors.Join(archive.ErrHistoryMutationPending, err)
	}
	return nil
}

func (s *sessionScan) checkHistoryPublication(p state.PendingPublication) error {
	var next archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &next); err != nil {
		return err
	}
	if p.History != nil {
		if p.History.Preparing {
			return archive.ErrHistoryMutationPending
		}
		_, err := s.checkFrozenHistoryMetadata(p)
		return err
	}
	if err := archive.CheckHistoryMutation(p.Bundle, next); err != nil {
		return err
	}
	if s.reg.Harness.Name != "codex" {
		return nil
	}
	// Lost local state cannot authorize flattening a readable remote history sidecar.
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var previous archive.Metadata
	if err := json.Unmarshal(raw, &previous); err != nil {
		return err
	}
	if err := archive.CheckHistoryMutation(archive.SourceBundle{}, previous); err != nil {
		return err
	}
	_, err = previous.SourceReferences()
	return err
}
