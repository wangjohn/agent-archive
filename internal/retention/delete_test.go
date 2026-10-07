package retention

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// deleteRecordingStore records the keys deleted, in order, and fails the
// delete of failKey.
type deleteRecordingStore struct {
	*storagetest.MemoryStore
	failKey string
	deleted []string
}

func (s *deleteRecordingStore) Delete(ctx context.Context, key string) error {
	if key == s.failKey {
		return errors.New("simulated delete failure")
	}
	s.deleted = append(s.deleted, key)
	return s.MemoryStore.Delete(ctx, key)
}

// DeleteWholeSession removes the metadata before any source object, so no
// live metadata ever points at missing data; when the metadata delete fails,
// it stops with an error and every source object is left in place.
func TestDeleteWholeSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const metadata = "sessions/claude/session-1/metadata.json"
	sources := []string{"sessions/claude/session-1/sources/a.json", "sessions/claude/session-1/sources/b.json"}
	other := "sessions/claude/session-2/metadata.json"
	// seed and keys take the subtest's t: the subtests run in parallel, after
	// this function has returned, so they must not fail through its t.
	seed := func(t *testing.T, failKey string) *deleteRecordingStore {
		t.Helper()
		store := &deleteRecordingStore{MemoryStore: storagetest.NewMemoryStore(), failKey: failKey}
		for _, key := range append([]string{metadata, other}, sources...) {
			if err := store.Put(ctx, key, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		return store
	}
	keys := func(t *testing.T, store *deleteRecordingStore) []string {
		t.Helper()
		objects, err := store.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, o := range objects {
			out = append(out, o.Key)
		}
		return out
	}

	t.Run("metadata first", func(t *testing.T) {
		t.Parallel()
		store := seed(t, "")
		if err := DeleteWholeSession(ctx, store, "claude", "session-1"); err != nil {
			t.Fatal(err)
		}
		if len(store.deleted) != 3 || store.deleted[0] != metadata {
			t.Fatalf("deleted %v, want the metadata first and then both sources", store.deleted)
		}
		if got := keys(t, store); !slices.Equal(got, []string{other}) {
			t.Fatalf("left %v, want only the other session", got)
		}
	})

	t.Run("failed source delete finishes on a rerun", func(t *testing.T) {
		t.Parallel()
		store := seed(t, sources[1])
		if err := DeleteWholeSession(ctx, store, "claude", "session-1"); err == nil {
			t.Fatal("a failed source delete was not reported")
		}
		// The metadata is gone, so nothing live points at the source left.
		if got := keys(t, store); !slices.Equal(got, []string{sources[1], other}) {
			t.Fatalf("left %v after the failure", got)
		}
		store.failKey = ""
		if err := DeleteWholeSession(ctx, store, "claude", "session-1"); err != nil {
			t.Fatalf("rerun: %v", err)
		}
		if got := keys(t, store); !slices.Equal(got, []string{other}) {
			t.Fatalf("left %v after the rerun", got)
		}
	})

	t.Run("failed metadata delete leaves the sources", func(t *testing.T) {
		t.Parallel()
		store := seed(t, metadata)
		if err := DeleteWholeSession(ctx, store, "claude", "session-1"); err == nil {
			t.Fatal("a failed metadata delete was not reported")
		}
		if len(store.deleted) != 0 {
			t.Fatalf("deleted %v after the metadata delete failed", store.deleted)
		}
		if got := keys(t, store); len(got) != 4 {
			t.Fatalf("left %v, want every object", got)
		}
	})
}

// Auxiliary cleanup must never keep expired transcripts alive.
func TestListingDeletionFailureStillDeletesSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const pointer = "listing/by-session/claude/session-1/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store := &deleteRecordingStore{MemoryStore: storagetest.NewMemoryStore(), failKey: pointer}
	for _, key := range []string{"sessions/claude/session-1/metadata.json", "sessions/claude/session-1/source.json", pointer} {
		if err := store.Put(ctx, key, []byte("damaged pointer")); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteWholeSession(ctx, store, "claude", "session-1"); err == nil {
		t.Fatal("auxiliary failure not reported")
	}
	objects, err := store.List(ctx, "sessions/claude/session-1/")
	if err != nil || len(objects) != 0 {
		t.Fatalf("auxiliary failure retained sources: %v %v", objects, err)
	}
}

func TestDeleteCompleteHistoryReopensAfterEveryObjectFailure(t *testing.T) {
	t.Parallel()
	const id = "history-delete"
	const native = "11111111-1111-4111-8111-111111111111"
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ref := func(digit string) archive.SourceReference {
		sha := strings.Repeat(digit, 64)
		return archive.SourceReference{Key: "sessions/codex/" + id + "/source." + sha + ".jsonl.gz", SHA256: sha, CompressedBytes: 10}
	}
	active, old := ref("a"), ref("b")
	metadataKey := "sessions/codex/" + id + "/metadata.json"
	stray := "sessions/codex/" + id + "/unreferenced-stage"
	m := archive.Metadata{SchemaVersion: archive.HistoryMetadataSchemaVersion, SessionID: id, NativeSessionID: native, Harness: archive.Harness{Name: "codex"}, ProjectID: "project-1", CapturedAt: at, SourceBundle: active, History: &archive.RevisionHistory{CurrentRevision: native, Preserved: []archive.RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: at.Add(time.Hour), Source: old}}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []string{metadataKey, active.Key, old.Key, stray} {
		t.Run(fail, func(t *testing.T) {
			t.Parallel()
			cloud := storagetest.NewMemoryStore()
			remote := &deleteRecordingStore{MemoryStore: cloud, failKey: fail}
			for _, key := range []string{active.Key, old.Key, stray} {
				if err := cloud.Put(t.Context(), key, []byte("synthetic")); err != nil {
					t.Fatal(err)
				}
			}
			if err := cloud.Put(t.Context(), metadataKey, raw); err != nil {
				t.Fatal(err)
			}
			if err := DeleteWholeSession(t.Context(), remote, "codex", id); err == nil {
				t.Fatal("deletion failure ignored")
			}
			if fail == metadataKey {
				for _, key := range []string{active.Key, old.Key, stray} {
					if _, err := cloud.Get(t.Context(), key); err != nil {
						t.Fatal("source removed before metadata", err)
					}
				}
			} else {
				if _, err := cloud.Get(t.Context(), metadataKey); !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("dangling sidecar", err)
				}
			}
			// A fresh deletion caller can finish an absent-sidecar prefix; no retained
			// native input, local journal or decoder is needed to own this exact prefix.
			retry := &deleteRecordingStore{MemoryStore: cloud}
			if err := DeleteWholeSession(t.Context(), retry, "codex", id); err != nil {
				t.Fatal(err)
			}
			objects, err := cloud.List(t.Context(), "sessions/codex/"+id)
			if err != nil || len(objects) != 0 {
				t.Fatal(objects, err)
			}
		})
	}
}
