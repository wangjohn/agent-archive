package collector

import (
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func (s *sessionScan) checkRetainedHistory() error {
	b, _, _, _ := s.published.Cached()
	var m archive.Metadata
	if raw := s.published.Metadata(); len(raw) > 0 {
		if err := s.unmarshalRetained(raw, &m); err != nil {
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

func (s *sessionScan) checkHistoryPublicationLocal(p state.PendingPublication) error {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	var next archive.Metadata
	if err := s.unmarshalRetained(p.MetadataBytes, &next); err != nil {
		return err
	}
	if p.History != nil {
		if p.History.Preparing {
			return archive.ErrHistoryMutationPending
		}
		_, err := s.frozenHistoryMetadata(p)
		return err
	}
	return archive.CheckHistoryMutation(p.Bundle, next)
}

func (s *sessionScan) checkHistoryPublicationBody(p state.PendingPublication, raw []byte) error {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	if err := s.checkHistoryPublicationLocal(p); err != nil {
		return err
	}
	if p.History != nil {
		if len(raw) == 0 {
			if p.History.ExpectedMetadataSHA256 != "" {
				return errHistoryMetadataConflict
			}
			return nil
		}
		if metadataSHA(raw) == metadataSHA(p.MetadataBytes) {
			return nil
		}
		if p.History.ExpectedMetadataSHA256 == "" || metadataSHA(raw) != p.History.ExpectedMetadataSHA256 {
			return errHistoryMetadataConflict
		}
		var previous archive.Metadata
		if err := s.unmarshalRetained(raw, &previous); err != nil {
			return err
		}
		if _, err := previous.SourceReferences(); err != nil {
			return err
		}
		return s.validateAuthorityIdentity(previous)
	}
	if s.reg.Harness.Name != "codex" || len(raw) == 0 {
		return nil
	}
	var previous archive.Metadata
	if err := s.unmarshalRetained(raw, &previous); err != nil {
		return err
	}
	if err := archive.CheckHistoryMutation(archive.SourceBundle{}, previous); err != nil {
		return err
	}
	_, err := previous.SourceReferences()
	return err
}

func (s *sessionScan) checkHistoryPublication(p state.PendingPublication) error {
	if err := s.checkHistoryPublicationLocal(p); err != nil {
		return err
	}
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if errors.Is(err, storage.ErrNotFound) {
		return s.checkHistoryPublicationBody(p, nil)
	}
	if err != nil {
		return err
	}
	return s.checkHistoryPublicationBody(p, raw)
}
