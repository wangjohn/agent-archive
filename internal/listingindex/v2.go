package listingindex

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// V2Prefix contains revision-qualified entries; coverage is checked every read.
const V2Prefix = "listing/v2/"

const v2Pointers = "listing/by-session-v2/"

// Revision contains only discovery summaries, never transcript or skill content.
type Revision struct {
	Key         string    `json:"-"`
	MetadataKey string    `json:"-"`
	CapturedAt  time.Time `json:"-"`
	Nonce       string    `json:"n,omitempty"`
	ETag        string    `json:"v"`
	Hash        string    `json:"h"`
	Activity    time.Time `json:"a"`
	Parent      string    `json:"p,omitempty"`
	Replay      bool      `json:"r,omitempty"`
	ProjectID   string    `json:"j"`
	RepoKey     string    `json:"k,omitempty"`
}

// ActivityTime matches the listing's activity-date policy.
func ActivityTime(m archive.Metadata) time.Time {
	if m.EndedAt != nil {
		return *m.EndedAt
	}
	if m.Origin == archive.SessionOriginImport && !m.StartedAt.IsZero() {
		return m.StartedAt
	}
	return m.CapturedAt
}

// NewRevision validates canonical identity and encodes an opaque validator.
// A fresh nonce prevents restoring identical bytes from reactivating a key
// which an older cleanup snapshot may already be deleting.
func NewRevision(key string, data []byte, etag string) (Revision, error) {
	return newRevision(key, data, etag, rand.Text())
}

func newRevision(key string, data []byte, etag, nonce string) (Revision, error) {
	legacy, err := New(key, data)
	if err != nil {
		return Revision{}, err
	}
	if etag == "" {
		return Revision{}, errors.New("store returned no metadata revision validator")
	}
	var m archive.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Revision{}, err
	}
	r := Revision{Nonce: nonce, MetadataKey: key, CapturedAt: m.CapturedAt, ETag: etag, Hash: legacy.Hash, Activity: ActivityTime(m), Parent: m.ParentSessionID, Replay: m.Replay != nil, ProjectID: m.ProjectID, RepoKey: m.RepoKey}
	encoded, err := json.Marshal(r)
	if err != nil {
		return Revision{}, err
	}
	components := strings.Split(strings.TrimPrefix(legacy.Key, Prefix), "/")
	r.Key = V2Prefix + strings.Join(components[:3], "/") + "/" + base64.RawURLEncoding.EncodeToString(encoded)
	if len(r.Key) > 1024 {
		return Revision{}, errors.New("listing entry exceeds object key limit")
	}
	return r, nil
}

// ParseRevision rejects unsupported and noncanonical encodings.
func ParseRevision(key string) (Revision, error) {
	if !strings.HasPrefix(key, V2Prefix) || len(key) > 1024 {
		return Revision{}, errors.New("unsupported listing entry")
	}
	parts := strings.Split(strings.TrimPrefix(key, V2Prefix), "/")
	if len(parts) != 4 {
		return Revision{}, errors.New("invalid listing entry")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return Revision{}, err
	}
	var r Revision
	if err = json.Unmarshal(encoded, &r); err != nil {
		return Revision{}, err
	}
	legacy, err := Parse(Prefix + strings.Join(parts[:3], "/") + "/" + r.Hash + ".json")
	if err != nil {
		return Revision{}, err
	}
	if r.ETag == "" || r.Activity.IsZero() || r.Nonce != "" && (len(r.Nonce) != 26 || strings.Trim(r.Nonce, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "") {
		return Revision{}, errors.New("invalid listing summary")
	}
	canonical, _ := json.Marshal(r)
	if base64.RawURLEncoding.EncodeToString(canonical) != parts[3] {
		return Revision{}, errors.New("noncanonical listing summary")
	}
	r.Key = key
	r.MetadataKey = legacy.MetadataKey
	r.CapturedAt = legacy.CapturedAt
	return r, nil
}

// ValidateMetadata checks identity, schema, digest and every discovery field
// against canonical bytes, preserving this immutable publication identity.
func (r Revision) ValidateMetadata(data []byte) error {
	check, err := newRevision(r.MetadataKey, data, r.ETag, r.Nonce)
	if err != nil {
		return err
	}
	if check.Key != r.Key {
		return errors.New("metadata does not match listing revision")
	}
	return nil
}

// SameSummary permits concurrent publications of the same canonical revision
// while rejecting different discovery claims for one provider validator.
func (r Revision) SameSummary(other Revision) bool {
	if r.MetadataKey != other.MetadataKey || !r.CapturedAt.Equal(other.CapturedAt) {
		return false
	}
	r.Nonce, other.Nonce = "", ""
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(other)
	return string(a) == string(b)
}

func revisionPointerPrefix(r Revision) string {
	parts := strings.Split(r.MetadataKey, "/")
	return v2Pointers + parts[1] + "/" + parts[2] + "/"
}

func revisionPointer(r Revision) string {
	return revisionPointerPrefix(r) + storage.SHA256Hex([]byte(r.Key))
}

// PutRevision writes the cleanup pointer first, then its empty immutable hint.
func PutRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	if _, err := ParseRevision(r.Key); err != nil {
		return err
	}
	if err := store.Put(ctx, revisionPointer(r), []byte(r.Key)); err != nil {
		return err
	}
	return store.Put(ctx, r.Key, nil)
}

// PublishRevision confirms the exact canonical bytes using a response validator.
// A concurrent replacement creates harmless stale hints, detected by coverage.
func PublishRevision(ctx context.Context, store storage.ObjectStore, key string, data []byte) error {
	getter, ok := store.(storage.VersionedGetter)
	if !ok {
		return errors.New("store cannot return metadata revision validators")
	}
	stored, etag, err := getter.GetVersioned(ctx, key)
	if err != nil {
		return err
	}
	if storage.SHA256Hex(stored) != storage.SHA256Hex(data) {
		return errors.New("canonical metadata changed during index publication")
	}
	r, err := NewRevision(key, stored, etag)
	if err != nil {
		return err
	}
	return RepairRevision(ctx, store, r)
}

// retireRevisions bounds cleanup writes per attempt. Remaining pointers make
// maintenance retryable; no source object is touched.
func retireRevisions(ctx context.Context, store storage.ObjectStore, current Revision, pointers []storage.Object) error {
	statter, ok := store.(storage.ObjectStatter)
	if !ok {
		return nil
	}
	// The candidates precede this fresh publication, so no concurrent writer
	// can retire it through an older snapshot, even after validator reuse.
	info, err := statter.Stat(ctx, current.MetadataKey)
	if err != nil {
		return err
	}
	if info.ETag != current.ETag {
		return errors.New("metadata changed before listing maintenance")
	}
	for i, p := range pointers {
		if i == 32 {
			return errors.New("listing cleanup remains pending")
		}
		data, err := store.Get(ctx, p.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		r, parseErr := ParseRevision(string(data))
		if parseErr == nil && r.MetadataKey == current.MetadataKey && revisionPointer(r) == p.Key {
			if err := store.Delete(ctx, r.Key); err != nil {
				return err
			}
		}
		if err := store.Delete(ctx, p.Key); err != nil {
			return err
		}
	}
	return nil
}

func deleteRevisions(ctx context.Context, store storage.ObjectStore, harness, id string) error {
	pointers, err := store.List(ctx, v2Pointers+harness+"/"+id+"/")
	if err != nil {
		return err
	}
	key, err := archive.MetadataObjectKey(harness, id)
	if err != nil {
		return err
	}
	for _, p := range pointers {
		data, err := store.Get(ctx, p.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		r, err := ParseRevision(string(data))
		if err == nil && r.MetadataKey == key && revisionPointer(r) == p.Key {
			if err = store.Delete(ctx, r.Key); err != nil {
				return fmt.Errorf("delete listing revision: %w", err)
			}
		}
		if err = store.Delete(ctx, p.Key); err != nil {
			return err
		}
	}
	return nil
}

// RepairRevision publishes a hint and retires at most 32 obsolete revisions.
func RepairRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	parsed, err := ParseRevision(r.Key)
	if err != nil {
		return err
	}
	r = parsed
	// Reusing a caller's revision after interruption is still a new repair
	// attempt. Only its summary is reused; its object identity must be fresh.
	r.Nonce = rand.Text()
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	parts := strings.Split(r.Key, "/")
	parts[len(parts)-1] = base64.RawURLEncoding.EncodeToString(encoded)
	r.Key = strings.Join(parts, "/")
	// Freeze retirement candidates before publishing this fresh identity.
	// Two concurrent snapshots cannot each contain the other's later entry:
	// one must precede it. The latest successful publication therefore survives
	// without a shared manifest, logical counter, clock or self-retirement.
	pointers, err := store.List(ctx, revisionPointerPrefix(r))
	if err != nil {
		return err
	}
	if err := PutRevision(ctx, store, r); err != nil {
		return err
	}
	return retireRevisions(ctx, store, r, pointers)
}

// DeleteRevision removes one validated auxiliary revision and its exact pointer.
func DeleteRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	if _, err := ParseRevision(r.Key); err != nil {
		return err
	}
	if err := store.Delete(ctx, r.Key); err != nil {
		return err
	}
	return store.Delete(ctx, revisionPointer(r))
}
