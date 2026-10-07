package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// pendingSourceStore overlays immutable checksum-bound replay bytes without native input.
type pendingSourceStore struct {
	storage.ObjectStore
	payloads map[string][]byte
}

func (s pendingSourceStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b, ok := s.payloads[key]; ok {
		return b, nil
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s pendingSourceStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if b, ok := s.payloads[key]; ok {
		if int64(len(b)) > limit {
			return nil, storage.ErrObjectTooLarge
		}
		return b, ctx.Err()
	}
	if getter, ok := s.ObjectStore.(storage.LimitedGetter); ok {
		return getter.GetLimited(ctx, key, limit)
	}
	b, err := s.Get(ctx, key)
	if int64(len(b)) > limit {
		return nil, storage.ErrObjectTooLarge
	}
	return b, err
}

func (s *sessionScan) pendingRetainedLoader(p state.PendingPublication) (retainedSourceLoader, error) {
	if err := p.ValidatePublication(); err != nil {
		return nil, err
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return nil, err
	}
	overlay := pendingSourceStore{ObjectStore: s.remote, payloads: map[string][]byte{}}
	if len(p.SourceBytes) > 0 {
		overlay.payloads[p.SourceKey] = p.SourceBytes
	}
	for _, source := range p.Sources {
		if len(source.Bytes) > 0 {
			overlay.payloads[source.Reference.Key] = source.Bytes
		}
	}
	return remoteRetainedLoader(overlay, metadata), nil
}

// reconcilePrivacyPending recognizes only original exact-next or explicit predecessor.
// Recording exact-next locally does not acknowledge requests or release admitted bytes.
func (s *sessionScan) reconcilePrivacyPending(p state.PendingPublication) (state.PublicationPredecessor, state.PrivacyAuthority, error) {
	if p.Commit == nil || p.Commit.Predecessor == state.PredecessorUnknown {
		return state.PublicationPredecessor{}, "", errors.New("obsolete pending privacy requires exact known sealed authority; reconcile retained evidence")
	}
	if err := p.ValidatePublication(); err != nil {
		return state.PublicationPredecessor{}, "", err
	}
	if p.Commit.DestinationID != s.reg.DestinationID || p.Commit.AdmissionContext != s.publicationAdmission() {
		return state.PublicationPredecessor{}, "", errors.New("pending destination or admission changed; retain evidence and reconcile")
	}
	remote, err := storage.ReadPublicationMetadata(s.ctx, s.remote, p.MetadataKey)
	if err == nil && storage.SHA256Hex(remote) == p.Commit.MetadataSHA256 {
		refs := make([]storage.SourcePublication, len(p.Sources))
		for i, source := range p.Sources {
			refs[i] = storage.SourcePublication{Key: source.Reference.Key, SHA256: source.Reference.SHA256, Size: source.Reference.CompressedBytes}
		}
		if err := storage.VerifySourceSet(s.ctx, s.remote, refs, s.opts.Retry); err != nil {
			return state.PublicationPredecessor{}, "", err
		}
		confirmed, err := storage.ReadPublicationMetadata(s.ctx, s.remote, p.MetadataKey)
		if err != nil {
			return state.PublicationPredecessor{}, "", err
		}
		if storage.SHA256Hex(confirmed) != p.Commit.MetadataSHA256 {
			return state.PublicationPredecessor{}, "", storage.ErrPublicationConflict
		}
		if err := s.published.SaveCommittedPublication(p, s.now); err != nil {
			return state.PublicationPredecessor{}, "", err
		}
		return s.published.PublicationPredecessor(), state.PrivacyCommitted, nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return state.PublicationPredecessor{}, "", err
	}
	matchesAbsent := errors.Is(err, storage.ErrNotFound) && p.Commit.Predecessor == state.PredecessorAbsent
	matchesPresent := err == nil && p.Commit.Predecessor == state.PredecessorPresent && storage.SHA256Hex(remote) == p.Commit.PredecessorSHA256
	if !matchesAbsent && !matchesPresent {
		return state.PublicationPredecessor{}, "", storage.ErrPublicationConflict
	}
	prior := s.published.PublicationPredecessor()
	if prior.State != p.Commit.Predecessor || prior.State == state.PredecessorPresent && storage.SHA256Hex(prior.Body) != p.Commit.PredecessorSHA256 {
		return prior, "", fmt.Errorf("pending prior local authority differs: %w", storage.ErrPublicationConflict)
	}
	prior.PrivacyPendingSource = &p
	return prior, state.PrivacyPending, nil
}

func (s *sessionScan) pendingPrivacyChanged(p state.PendingPublication) bool {
	if pendingSkillMode(p.SkillEvidence) != s.opts.skillEvidence() {
		return true
	}
	if _, err := s.publicationPolicy(p.Bundle); err != nil {
		return true
	}
	if p.Commit == nil || p.Commit.Privacy == nil || p.Commit.Privacy.InputJournalSHA256 == "" {
		if _, err := s.local.ReadPublicationEvidence(s.id()); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}
