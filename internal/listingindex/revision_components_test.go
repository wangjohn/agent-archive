package listingindex_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type componentLimitedStore struct{ *storagetest.MemoryStore }

func (s *componentLimitedStore) Put(ctx context.Context, key string, data []byte) error {
	for component := range strings.SplitSeq(key, "/") {
		if len(component) > 255 {
			return fmt.Errorf("object component exceeds 255 bytes")
		}
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func revisionMetadata(t *testing.T) (string, []byte) {
	t.Helper()
	key := "sessions/codex/session/metadata.json"
	m := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: "session", ProjectID: "12345678-1234-1234-1234-123456789abc", RepoKey: strings.Repeat("b", 64), Harness: archive.Harness{Name: "codex"}, CapturedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), SourceBundle: archive.SourceReference{Key: "source", SHA256: strings.Repeat("a", 64)}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return key, data
}

func TestRevisionPublicationFitsFilesystemObjectComponents(t *testing.T) {
	ctx := context.Background()
	s := &componentLimitedStore{storagetest.NewMemoryStore()}
	key, data := revisionMetadata(t)
	if err := s.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	_, etag, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	r, err := listingindex.NewRevision(key, data, etag)
	if err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PutRevision(ctx, s, r); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(ctx, s, key, data); err != nil {
		t.Fatal(err)
	}
	hints, err := s.List(ctx, listingindex.V3Prefix)
	if err != nil || len(hints) != 1 {
		t.Fatalf("hints=%v err=%v", hints, err)
	}
	parsed, err := listingindex.ParseRevision(hints[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.SameSummary(r) {
		t.Fatal("summary changed during repair")
	}
	if err := parsed.ValidateMetadata(data); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.DeleteSession(ctx, s, "codex", "session"); err != nil {
		t.Fatal(err)
	}
	hints, err = s.List(ctx, listingindex.V3Prefix)
	if err != nil || len(hints) != 0 {
		t.Fatalf("cleanup hints=%v err=%v", hints, err)
	}
}

func legacyRevisionKey(r listingindex.Revision, v2 bool) string {
	parts := strings.Split(strings.TrimPrefix(r.Key, listingindex.V3Prefix), "/")
	summary := strings.Join(parts[3:], "")
	if v2 {
		return listingindex.V2Prefix + parts[2] + "/" + parts[0] + "/" + parts[1] + "/" + summary
	}
	return listingindex.V3Prefix + strings.Join(parts[:3], "/") + "/" + summary
}

func TestLegacyRevisionReadRepairAndCleanupKeepIdentity(t *testing.T) {
	ctx := context.Background()
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%v", v2), func(t *testing.T) {
			s := &componentLimitedStore{storagetest.NewMemoryStore()}
			key, data := revisionMetadata(t)
			if err := s.Put(ctx, key, data); err != nil {
				t.Fatal(err)
			}
			_, etag, err := s.GetVersioned(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			current, err := listingindex.NewRevision(key, data, etag)
			if err != nil {
				t.Fatal(err)
			}
			oldKey := legacyRevisionKey(current, v2)
			old, err := listingindex.ParseRevision(oldKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := listingindex.PutRevision(ctx, s, old); err == nil {
				t.Fatal("writer accepted legacy oversized encoding")
			}
			if !current.SameSummary(old) {
				t.Fatal("legacy identity differs")
			}
			if err := old.ValidateMetadata(data); err != nil {
				t.Fatal(err)
			}
			// Seed artifacts through an unrestricted old store; the repaired writer
			// must obey the filesystem limit even when reading earlier S3 keys.
			if err := s.MemoryStore.Put(ctx, oldKey, nil); err != nil {
				t.Fatal(err)
			}
			legacy, err := listingindex.LegacyEntries(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if err := listingindex.RebuildRevision(ctx, s, old, legacy[key]); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(ctx, oldKey); err == nil {
				t.Fatal("old encoding survived repair")
			}
			hints, err := s.List(ctx, listingindex.V3Prefix)
			if err != nil || len(hints) != 1 {
				t.Fatalf("hints=%v err=%v", hints, err)
			}
			repaired, err := listingindex.ParseRevision(hints[0].Key)
			if err != nil || !current.SameSummary(repaired) || repaired.Nonce == old.Nonce {
				t.Fatalf("repaired=%+v err=%v", repaired, err)
			}
		})
	}
}

func TestRevisionRefusesNoncanonicalSummaryComponents(t *testing.T) {
	key, data := revisionMetadata(t)
	r, err := listingindex.NewRevision(key, data, "opaque-validator")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(r.Key, listingindex.V3Prefix), "/")
	prefix := listingindex.V3Prefix + strings.Join(parts[:3], "/") + "/"
	summary := strings.Join(parts[3:], "")
	for _, suffix := range []string{
		summary[:254] + "/" + summary[254:],
		summary[:256] + "/" + summary[256:],
		strings.Join(parts[3:], "/") + "/",
		"/" + strings.Join(parts[3:], "/"),
		parts[3] + "//" + strings.Join(parts[4:], "/"),
		strings.Join(parts[3:], "/") + "=",
	} {
		if _, err := listingindex.ParseRevision(prefix + suffix); err == nil {
			t.Fatalf("accepted malformed suffix %q", suffix)
		}
	}
	v2 := listingindex.V2Prefix + parts[2] + "/" + parts[0] + "/" + parts[1] + "/" + strings.Join(parts[3:], "/")
	if _, err := listingindex.ParseRevision(v2); err == nil {
		t.Fatal("accepted segmented v2")
	}
}

func TestRevisionKeyBoundsIncludeChunkSeparators(t *testing.T) {
	key, data := revisionMetadata(t)
	for size := 1; size < 1000; size++ {
		r, err := listingindex.NewRevision(key, data, strings.Repeat("v", size))
		if err != nil {
			previous, previousErr := listingindex.NewRevision(key, data, strings.Repeat("v", size-1))
			if previousErr != nil {
				t.Fatal(previousErr)
			}
			if len(previous.Key) < 1022 {
				t.Fatalf("premature key bound: %d, %v", len(previous.Key), err)
			}
			// The old single-component form near the bound must remain readable
			// and validate without attempting a larger chunked re-encoding.
			old := previous
			validated := 0
			for extra := 1; extra <= 4; extra++ {
				old.ETag = previous.ETag + strings.Repeat("v", extra)
				encoded, err := json.Marshal(old)
				if err != nil {
					t.Fatal(err)
				}
				prefix := strings.Join(strings.Split(legacyRevisionKey(previous, false), "/")[:5], "/") + "/"
				oldKey := prefix + base64.RawURLEncoding.EncodeToString(encoded)
				if len(oldKey) > 1024 {
					if _, err := listingindex.ParseRevision(oldKey); err == nil {
						t.Fatal("reader accepted oversized key")
					}
					continue
				}
				parsed, err := listingindex.ParseRevision(oldKey)
				if err != nil {
					t.Fatal(err)
				}
				if err := parsed.ValidateMetadata(data); err != nil {
					t.Fatal(err)
				}
				validated++
			}
			if validated == 0 {
				t.Fatal("no near-bound legacy keys validated")
			}
			return
		}
		if len(r.Key) > 1024 {
			t.Fatal("oversized key")
		}
		for component := range strings.SplitSeq(r.Key, "/") {
			if len(component) > 255 {
				t.Fatal("oversized component")
			}
		}
	}
	t.Fatal("object key bound not enforced")
}

func TestRevisionWriterRefusesOversizedIdentityComponents(t *testing.T) {
	_, data := revisionMetadata(t)
	var m archive.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.SessionID = strings.Repeat("s", 256)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	key := "sessions/codex/" + m.SessionID + "/metadata.json"
	if _, err := listingindex.NewRevision(key, data, "validator"); err == nil || !strings.Contains(err.Error(), "component limit") {
		t.Fatalf("oversized identity: %v", err)
	}
}

// Unresolved native children must stay children through the segmented summary,
// including canonical validation and cleanup on component-limited object stores.
func TestSegmentedRevisionKeepsNativeChildOwnership(t *testing.T) {
	t.Parallel()
	key, data := revisionMetadata(t)
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.NativeChild = true
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	store := &componentLimitedStore{storagetest.NewMemoryStore()}
	if err := store.Put(t.Context(), key, data); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(t.Context(), store, key, data); err != nil {
		t.Fatal(err)
	}
	hints, err := store.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 1 {
		t.Fatal(hints, err)
	}
	parsed, err := listingindex.ParseRevision(hints[0].Key)
	if err != nil || !parsed.NativeChild || parsed.Parent != "" {
		t.Fatal("native child ownership lost in segmented summary", parsed, err)
	}
	for component := range strings.SplitSeq(hints[0].Key, "/") {
		if len(component) > 255 {
			t.Fatal("component too large")
		}
	}
	if err := parsed.ValidateMetadata(data); err != nil {
		t.Fatal(err)
	}
	metadata.NativeChild = false
	changed, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := parsed.ValidateMetadata(changed); err == nil {
		t.Fatal("changed child identity accepted")
	}
	if err := listingindex.DeleteSession(t.Context(), store, "codex", "session"); err != nil {
		t.Fatal(err)
	}
	hints, err = store.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 0 {
		t.Fatal("native child listing cleanup failed", hints, err)
	}
}
