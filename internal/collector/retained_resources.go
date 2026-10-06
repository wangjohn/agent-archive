package collector

import (
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
)

var errRetainedBudget = errors.New("retained work exceeds shared data budget")

func (s *sessionScan) readBudget() *agentapi.NativeReadBudget {
	if s.opts.sourcePasses != nil {
		return s.opts.sourcePasses.env.ReadBudget
	}
	if s.retainedBudget == nil {
		if shared, ok := s.opts.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok {
			s.retainedBudget = shared.NativeReadBudget()
		}
		if s.retainedBudget == nil {
			s.retainedBudget = agentapi.NewNativeReadBudget(128 << 20)
		}
	}
	return s.retainedBudget
}

// releaseRetained ends the scan's ownership of retained logical data. Scratch
// used only during a bounded operation is released by that operation instead.
func (s *sessionScan) releaseRetained() {
	for i := len(s.retainedReleases) - 1; i >= 0; i-- {
		s.retainedReleases[i]()
	}
	s.retainedReleases = nil
}

func (s *sessionScan) retainCharge(n int64) error {
	b := s.readBudget()
	if !b.Reserve(n) {
		return errRetainedBudget
	}
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	return nil
}

func (s *sessionScan) historyGet(key string, limit int64) ([]byte, error) {
	b := s.readBudget()
	if limit <= 0 || limit > historyCompressedLimit {
		return nil, errRetainedBudget
	}
	if !b.Reserve(limit) {
		return nil, errRetainedBudget
	}
	data, err := historyLimitedGet(s.ctx, s.remote, key, limit)
	if err != nil {
		b.Release(limit)
		return nil, err
	}
	n := int64(len(data))
	b.Release(limit - n)
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	return data, nil
}

func (s *sessionScan) historyStage(stage state.PendingSource) ([]byte, error) {
	n := int64(stage.Reference.CompressedBytes)
	if n <= 0 || n > historyCompressedLimit {
		return nil, errRetainedBudget
	}
	b := s.readBudget()
	if !b.Reserve(n) {
		return nil, errRetainedBudget
	}
	data, err := s.local.ReadPendingSource(s.id(), stage)
	if err != nil {
		b.Release(n)
		return nil, err
	}
	s.retainedReleases = append(s.retainedReleases, func() { b.Release(n) })
	return data, nil
}

func (s *sessionScan) decodeReferenced(metadata archive.Metadata, data []byte) (archive.SourceBundle, error) {
	bundle, release, err := reader.DecodeReferencedSourceLeased(s.ctx, metadata, data, reader.Limits{}, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return bundle, err
}

func (s *sessionScan) decodeRevision(metadata archive.Metadata, revision string, data []byte) (archive.SourceBundle, error) {
	bundle, release, err := reader.DecodeRevisionSourceLeased(s.ctx, metadata, revision, data, reader.Limits{}, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return bundle, err
}
