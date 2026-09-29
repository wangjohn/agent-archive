package reader

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type indexedCountingStore struct {
	*countingStore
	pages int
}

func (s *indexedCountingStore) ListPage(ctx context.Context, prefix, continuation string, limit int32) (storage.ObjectPage, error) {
	s.pages++
	return s.MemoryStore.ListPage(ctx, prefix, continuation, limit)
}

func TestListRecentStopsAfterVerifiedLimitAndKeepsHonestCount(t *testing.T) {
	ctx := context.Background()
	store := &indexedCountingStore{countingStore: newCountingStore()}
	for i := range 300 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*time.Minute))
	}
	if got, err := RebuildIndex(ctx, store, "sessions"); err != nil || got != 300 {
		t.Fatalf("rebuild=%d,%v", got, err)
	}
	store.reset()
	got, err := ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Complete || len(got.Sessions) != 50 || !got.Sessions[0].CapturedAt.Equal(baseTime.Add(299*time.Minute)) {
		t.Fatalf("recent=%+v", got)
	}
	_, gets := store.counts()
	if store.pages != 1 || len(gets) != 51 {
		t.Fatalf("remote work: pages=%d gets=%d", store.pages, len(gets))
	}
	all, err := ListRecent(ctx, store, "sessions", Filter{}, 0, ListOptions{})
	if err != nil || !all.Complete || all.TotalMatched != 300 {
		t.Fatalf("full result: count=%d complete=%v err=%v", all.TotalMatched, all.Complete, err)
	}
}

func TestListRecentIgnoresStaleIndexAndDeletedMetadata(t *testing.T) {
	ctx := context.Background()
	store := &indexedCountingStore{countingStore: newCountingStore()}
	old := putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	putSession(t, store, "codex", fmt.Sprintf("%032x", 2), baseTime.Add(time.Hour))
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	// A replacement sidecar invalidates its old hash. Until the matching hint
	// is published, the reader falls back to the authoritative full scan.
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime.Add(2*time.Hour))
	got, err := ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{})
	if err != nil || len(got.Sessions) != 2 || got.Sessions[0].SessionID != fmt.Sprintf("%032x", 1) {
		t.Fatalf("stale result=%+v,%v", got, err)
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	got, err = ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{})
	if err != nil || len(got.Sessions) != 2 || got.Sessions[0].SessionID != fmt.Sprintf("%032x", 1) {
		t.Fatalf("rebuild result=%+v,%v", got, err)
	}
	if err := store.Delete(ctx, old); err != nil {
		t.Fatal(err)
	}
	got, err = ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{})
	if err != nil || len(got.Sessions) != 1 {
		t.Fatalf("deleted result=%d,%v", len(got.Sessions), err)
	}
	if ready, err := listingindex.Ready(ctx, store); err != nil || !ready {
		t.Fatalf("ready=%v,%v", ready, err)
	}
}

func TestListRecentFallsBackForDamagedIndexKey(t *testing.T) {
	ctx := context.Background()
	store := &indexedCountingStore{countingStore: newCountingStore()}
	putSession(t, store, "codex", fmt.Sprintf("%032x", 1), baseTime)
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, listingindex.Prefix+"bad-key", []byte("damaged")); err != nil {
		t.Fatal(err)
	}
	got, err := ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{})
	if err != nil || !got.Complete || got.TotalMatched != 1 {
		t.Fatalf("fallback=%+v,%v", got, err)
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, listingindex.Prefix+"bad-key"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("malformed hint survived rebuild: %v", err)
	}
	store.reset()
	store.pages = 0
	got, err = ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{})
	if err != nil || !got.Complete || got.TotalMatched != 1 || store.pages != 1 {
		t.Fatalf("repaired index=%+v pages=%d err=%v", got, store.pages, err)
	}
}

func TestListRecentSinceStopsAtBoundary(t *testing.T) {
	ctx := context.Background()
	store := &indexedCountingStore{countingStore: newCountingStore()}
	for i := range 300 {
		putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i)*time.Minute))
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	store.reset()
	got, err := ListRecent(ctx, store, "sessions", Filter{From: baseTime.Add(250 * time.Minute)}, 50, ListOptions{})
	if err != nil || !got.Complete || got.TotalMatched != 50 {
		t.Fatalf("since=%+v,%v", got, err)
	}
	_, gets := store.counts()
	if len(gets) != 50 {
		t.Fatalf("since read %d objects; want 50 sidecars", len(gets))
	}
}
