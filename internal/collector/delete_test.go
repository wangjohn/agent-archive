package collector

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// deleteRecordingStore records the keys deleted, in order, and fails the
// delete of failKey.
type deleteRecordingStore struct {
	*storage.MemoryStore
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
	ctx := context.Background()
	const metadata = "sessions/claude/session-1/metadata.json"
	sources := []string{"sessions/claude/session-1/sources/a.json", "sessions/claude/session-1/sources/b.json"}
	other := "sessions/claude/session-2/metadata.json"
	seed := func(failKey string) *deleteRecordingStore {
		store := &deleteRecordingStore{MemoryStore: storage.NewMemoryStore(), failKey: failKey}
		for _, key := range append([]string{metadata, other}, sources...) {
			if err := store.Put(ctx, key, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		return store
	}
	keys := func(store *deleteRecordingStore) []string {
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
		store := seed("")
		if err := DeleteWholeSession(ctx, store, "claude", "session-1"); err != nil {
			t.Fatal(err)
		}
		if len(store.deleted) != 3 || store.deleted[0] != metadata {
			t.Fatalf("deleted %v, want the metadata first and then both sources", store.deleted)
		}
		if got := keys(store); !slices.Equal(got, []string{other}) {
			t.Fatalf("left %v, want only the other session", got)
		}
	})

	t.Run("failed metadata delete leaves the sources", func(t *testing.T) {
		store := seed(metadata)
		if err := DeleteWholeSession(ctx, store, "claude", "session-1"); err == nil {
			t.Fatal("a failed metadata delete was not reported")
		}
		if len(store.deleted) != 0 {
			t.Fatalf("deleted %v after the metadata delete failed", store.deleted)
		}
		if got := keys(store); len(got) != 4 {
			t.Fatalf("left %v, want every object", got)
		}
	})
}
