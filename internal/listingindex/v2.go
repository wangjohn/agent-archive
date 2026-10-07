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

// V3Prefix stores each immutable revision under its own session for cleanup.
const V3Prefix = "listing/v3/"

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
	r, err := revisionSummary(key, data, etag, nonce)
	if err != nil {
		return Revision{}, err
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return Revision{}, err
	}
	r.Key, err = portableRevisionKey(r.MetadataKey, r.CapturedAt, encoded)
	if err != nil {
		return Revision{}, err
	}
	return r, nil
}

// revisionSummary validates canonical metadata independently of new hint write
// limits, so existing readable single-component hints retain their semantics.
func revisionSummary(key string, data []byte, etag, nonce string) (Revision, error) {
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
	return r, nil
}

// revisionComponentBytes leaves room below filesystem-backed provider limits.
// Old long single-component hints remain readable, but are never regenerated.
const revisionComponentBytes = 240

func portableRevisionKey(metadataKey string, capturedAt time.Time, encoded []byte) (string, error) {
	parts := strings.Split(metadataKey, "/")
	if len(parts) != 4 || parts[0] != "sessions" || parts[3] != "metadata.json" || len(parts[1]) > revisionComponentBytes || len(parts[2]) > revisionComponentBytes {
		return "", errors.New("invalid listing metadata identity")
	}
	reverse := fmt.Sprintf("%019d", maxTime-uint64(capturedAt.UnixNano()))
	key := V3Prefix + parts[1] + "/" + parts[2] + "/" + reverse + "/"
	payloadLength := base64.RawURLEncoding.EncodedLen(len(encoded))
	overhead := 0
	if payloadLength > revisionComponentBytes {
		overhead = 2 + (payloadLength-1)/revisionComponentBytes
	}
	if len(key)+payloadLength+overhead > 1024 {
		return "", errors.New("listing entry exceeds object key limit")
	}
	payload := base64.RawURLEncoding.EncodeToString(encoded)
	if len(payload) <= revisionComponentBytes {
		key += payload
	} else {
		var chunks []string
		for len(payload) > revisionComponentBytes {
			chunks = append(chunks, payload[:revisionComponentBytes])
			payload = payload[revisionComponentBytes:]
		}
		chunks = append(chunks, payload)
		key += "c/" + strings.Join(chunks, "/")
	}
	if len(key) > 1024 {
		return "", errors.New("listing entry exceeds object key limit")
	}
	return key, nil
}

// revisionKeyParts normalizes supported layouts to the historical date-first
// identity, validating chunk boundaries before decoding the joined summary.
func revisionKeyParts(key string) ([]string, bool, error) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(key, V2Prefix), V3Prefix), "/")
	chunked := strings.HasPrefix(key, V3Prefix) && len(parts) >= 5 && parts[3] == "c"
	if chunked {
		chunks := parts[4:]
		for i, chunk := range chunks {
			if len(chunk) == 0 || len(chunk) > revisionComponentBytes || i < len(chunks)-1 && len(chunk) != revisionComponentBytes {
				return nil, false, errors.New("noncanonical listing chunks")
			}
		}
		payload := strings.Join(chunks, "")
		if len(payload) <= revisionComponentBytes {
			return nil, false, errors.New("unnecessary listing chunks")
		}
		parts = []string{parts[2], parts[0], parts[1], payload}
	} else if strings.HasPrefix(key, V3Prefix) && len(parts) == 4 {
		parts[0], parts[1], parts[2] = parts[2], parts[0], parts[1]
	}
	if len(parts) != 4 || len(parts[0]) != 19 {
		return nil, false, errors.New("invalid listing entry")
	}
	return parts, chunked, nil
}

// ParseRevision rejects unsupported and noncanonical encodings.
func ParseRevision(key string) (Revision, error) {
	if (!strings.HasPrefix(key, V2Prefix) && !strings.HasPrefix(key, V3Prefix)) || len(key) > 1024 {
		return Revision{}, errors.New("unsupported listing entry")
	}
	parts, chunked, err := revisionKeyParts(key)
	if err != nil {
		return Revision{}, err
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
	if chunked {
		expected, err := portableRevisionKey(legacy.MetadataKey, legacy.CapturedAt, canonical)
		if err != nil || expected != key {
			return Revision{}, errors.New("noncanonical listing chunk layout")
		}
	}
	r.Key = key
	r.MetadataKey = legacy.MetadataKey
	r.CapturedAt = legacy.CapturedAt
	return r, nil
}

// ValidateMetadata checks identity, schema, digest and every discovery field
// against canonical bytes, preserving this immutable publication identity.
func (r Revision) ValidateMetadata(data []byte) error {
	check, err := revisionSummary(r.MetadataKey, data, r.ETag, r.Nonce)
	if err != nil {
		return err
	}
	if !check.SameSummary(r) {
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

// PutRevision writes one immutable session-addressed v3 hint.
func PutRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	parsed, err := ParseRevision(r.Key)
	if err != nil {
		return err
	}
	r = parsed
	if !strings.HasPrefix(r.Key, V3Prefix) {
		return errors.New("legacy listing revisions are read-only")
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	portable, err := portableRevisionKey(r.MetadataKey, r.CapturedAt, encoded)
	if err != nil {
		return err
	}
	if portable != r.Key {
		return errors.New("listing revision requires portable encoding")
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

// RepairRevision publishes a fresh session-addressed hint and retires at most
// 32 earlier entries. Its snapshot cannot include a later writer's identity.
func RepairRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	return repairRevision(ctx, store, r, nil)
}

func repairRevision(ctx context.Context, store storage.ObjectStore, r Revision, legacy []storage.Object) error {
	parsed, err := ParseRevision(r.Key)
	if err != nil {
		return err
	}
	r = parsed
	r.Nonce = rand.Text()
	encoded, err := json.Marshal(r)
	if err != nil {
		return err
	}
	r.Key, err = portableRevisionKey(r.MetadataKey, r.CapturedAt, encoded)
	if err != nil {
		return err
	}
	candidates, err := store.List(ctx, revisionSessionPrefix(r.MetadataKey))
	if err != nil {
		return err
	}
	candidates = append(candidates, legacy...)
	if err := PutRevision(ctx, store, r); err != nil {
		return err
	}
	if statter, ok := store.(storage.ObjectStatter); ok {
		info, err := statter.Stat(ctx, r.MetadataKey)
		if err != nil {
			return err
		}
		if info.ETag != r.ETag {
			return errors.New("metadata changed before listing maintenance")
		}
	} else if len(candidates) != 0 {
		return errors.New("store cannot confirm listing maintenance validator")
	}
	return retireCandidates(ctx, store, r.MetadataKey, candidates)
}

func revisionSessionPrefix(metadataKey string) string {
	parts := strings.Split(metadataKey, "/")
	return V3Prefix + parts[1] + "/" + parts[2] + "/"
}

// DeleteRevision removes one validated auxiliary revision and its exact pointer.
func DeleteRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	parsed, err := ParseRevision(r.Key)
	if err != nil {
		return err
	}
	r = parsed
	if err := store.Delete(ctx, r.Key); err != nil {
		return err
	}
	if strings.HasPrefix(r.Key, V3Prefix) {
		return nil
	}
	return store.Delete(ctx, revisionPointer(r))
}
