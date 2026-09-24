// Package statetest holds whole-file helpers over a session's published
// state for tests: each loads the state, makes one change, and writes it
// back. Production code must not use them. A collector scan keeps one
// state.Published per session for the whole scan, and a save that bypassed
// it would be overwritten, or silently ignored, by the scan's own copy.
package statetest

import (
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// SavePublished is state.Published.Save for one session.
func SavePublished(s *state.Store, archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, status state.CacheStatus, metadata ...[]byte) error {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.Save(bundle, publishedAt, status, metadata...)
}

// SavePublication is state.Published.SavePublication for one session.
func SavePublication(s *state.Store, archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, source archive.SourceReference, metadata []byte) error {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.SavePublication(bundle, publishedAt, source, metadata)
}

// SaveBlocked is state.Published.SaveBlocked for one session.
func SaveBlocked(s *state.Store, archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, reason state.BlockedReason, deferred ...archive.SupplementalEvidence) error {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.SaveBlocked(bundle, publishedAt, reason, deferred...)
}

// ClearRecoverableBlock is state.Published.ClearRecoverableBlock for one
// session.
func ClearRecoverableBlock(s *state.Store, archiveSessionID string, now time.Time) (restored state.CacheStatus, replayed, cleared bool, err error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return "", false, false, err
	}
	return p.ClearRecoverableBlock(now)
}

// CacheMetadata is state.Published.CacheMetadata for one session.
func CacheMetadata(s *state.Store, archiveSessionID string, metadata []byte) error {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return err
	}
	return p.CacheMetadata(metadata)
}
