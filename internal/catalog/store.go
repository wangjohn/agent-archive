package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ErrGCRequired defers physical cleanup until global GC fencing is held.
var ErrGCRequired = errors.New("catalog source deletion requires fenced garbage collection")

// Store is the narrow publication/history authority adapter. General catalog
// query activation belongs to migration; no canonical metadata is dual-written.
type Store struct {
	storage.ObjectStore
	Writer *Writer
}

// Wrap installs catalog metadata authority over a qualified object store.
func Wrap(store storage.ObjectStore) (*Store, error) {
	w, err := New(store)
	if err != nil {
		return nil, err
	}
	return &Store{store, w}, nil
}

func metadataKey(key string) bool {
	return strings.HasPrefix(key, "sessions/") && strings.HasSuffix(key, "/metadata.json")
}

// Get resolves catalog metadata and delegates immutable source reads.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	return s.GetLimited(ctx, key, 64<<20)
}

// GetLimited preserves bounded allocation for metadata and source reads.
func (s *Store) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if !metadataKey(key) {
		return s.Writer.bounded.GetLimited(ctx, key, limit)
	}
	entry, _, err := s.Writer.Find(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, storage.ErrNotFound
	}
	return s.Writer.readRef(ctx, entry.Metadata, limit)
}

// GetVersioned uses a unique catalog revision as metadata validator.
func (s *Store) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	return s.GetLimitedVersioned(ctx, key, 64<<20)
}

// GetLimitedVersioned preserves a bounded body and exact revision.
func (s *Store) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if !metadataKey(key) {
		raw, v, err := s.Writer.versioned.GetCatalogVersion(ctx, key, limit)
		return raw, v.ETag, err
	}
	entry, revision, err := s.Writer.Find(ctx, key)
	if err != nil {
		return nil, "", err
	}
	if entry == nil {
		return nil, "", storage.ErrNotFound
	}
	raw, err := s.Writer.readRef(ctx, entry.Metadata, limit)
	return raw, revision, err
}

// Stat describes current catalog metadata or delegates source checksums.
func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if !metadataKey(key) {
		if statter, ok := s.ObjectStore.(storage.ObjectStatter); ok {
			return statter.Stat(ctx, key)
		}
	}
	raw, etag, err := s.GetVersioned(ctx, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return storage.ObjectInfo{ETag: etag, Size: int64(len(raw)), SHA256: storage.SHA256Hex(raw)}, nil
}

// ListPage delegates the underlying paged listing capability.
func (s *Store) ListPage(ctx context.Context, prefix, continuation string, limit int32) (storage.ObjectPage, error) {
	if l, ok := s.ObjectStore.(storage.PageLister); ok {
		return l.ListPage(ctx, prefix, continuation, limit)
	}
	return storage.ObjectPage{}, errors.New("underlying store does not support page listing")
}

// ListRange delegates the underlying range listing capability.
func (s *Store) ListRange(ctx context.Context, prefix, after, through string) ([]storage.Object, error) {
	if l, ok := s.ObjectStore.(storage.RangeLister); ok {
		return l.ListRange(ctx, prefix, after, through)
	}
	return nil, errors.New("underlying store does not support range listing")
}

// ObjectKey preserves configured provider key composition.
func (s *Store) ObjectKey(key string) string {
	if k, ok := s.ObjectStore.(storage.ObjectKeyer); ok {
		return k.ObjectKey(key)
	}
	return key
}

// Put refuses unfrozen metadata and creates immutable source bytes.
func (s *Store) Put(ctx context.Context, key string, raw []byte) error {
	if metadataKey(key) {
		return errors.New("catalog metadata requires a frozen mutation")
	}
	if strings.HasPrefix(key, "sessions/") {
		_, err := s.Writer.conditional.PutConditional(ctx, key, raw, storage.PutCondition{CreateOnly: true})
		if err != nil {
			existing, e := s.Writer.bounded.GetLimited(ctx, key, int64(len(raw)))
			if e != nil {
				return errors.Join(err, e)
			}
			if string(existing) != string(raw) {
				return storage.ErrChecksumMismatch
			}
		}
		return nil
	}
	return s.ObjectStore.Put(ctx, key, raw)
}

// Delete refuses unfenced catalog and session object removal.
func (s *Store) Delete(ctx context.Context, key string) error {
	if strings.HasPrefix(key, "sessions/") || strings.HasPrefix(key, "catalog-v4/") {
		return ErrGCRequired
	}
	return s.ObjectStore.Delete(ctx, key)
}

// Publication binds the already persisted mutation to the metadata write made
// by the existing source-first machinery. Other metadata keys are refused.
func (s *Store) Publication(id, key, expected string) storage.ObjectStore {
	return publicationStore{s, id, key, expected}
}

type publicationStore struct {
	*Store
	id       string
	key      string
	expected string
}

func (s publicationStore) Put(ctx context.Context, key string, raw []byte) error {
	if !metadataKey(key) {
		return s.Store.Put(ctx, key, raw)
	}
	if key != s.key {
		return errors.New("publication metadata key changed")
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	canonical, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil || canonical != key {
		return errors.New("catalog metadata identity mismatch")
	}
	if _, err = metadata.SourceReferences(); err != nil {
		return err
	}
	ref, err := s.Writer.PutImmutable(ctx, KindMetadata, raw)
	if err != nil {
		return err
	}
	_, err = s.Writer.Commit(ctx, CatalogMutation{ID: s.id, SessionKey: key, ExpectedRevision: s.expected, Next: &CatalogEntry{Metadata: ref, Summary: metadata}})
	return err
}

// DeleteSession commits a tombstone before any physical source cleanup.
func (s *Store) DeleteSession(ctx context.Context, key string) error {
	entry, revision, err := s.Writer.Find(ctx, key)
	if err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	id, err := NewMutationID()
	if err != nil {
		return err
	}
	_, err = s.Writer.Commit(ctx, CatalogMutation{ID: id, SessionKey: key, ExpectedRevision: revision})
	return err
}

// CatalogMetadataAuthority reports that canonical metadata resolves via head.
func (s *Store) CatalogMetadataAuthority() bool { return true }

// CatalogBarrier obtains the underlying destination coordinator's complete
// global writer/history/pending barrier. A missing coordinator never grants GC.
func (s *Store) CatalogBarrier(ctx context.Context) (Barrier, error) {
	provider, ok := s.ObjectStore.(interface {
		CatalogBarrier(context.Context) (Barrier, error)
	})
	if !ok {
		return nil, errors.New("complete catalog destination coordinator is required")
	}
	barrier, err := provider.CatalogBarrier(ctx)
	if err != nil {
		return nil, err
	}
	if barrier == nil {
		return nil, errors.New("complete catalog destination coordinator is required")
	}
	return barrier, nil
}

// FreezeCatalogMutation observes the current revision before durable journaling.
func (s *Store) FreezeCatalogMutation(ctx context.Context, key string) (string, string, error) {
	_, revision, err := s.Writer.Find(ctx, key)
	if err != nil {
		return "", "", err
	}
	id, err := NewMutationID()
	return id, revision, err
}
