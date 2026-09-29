package listingindex_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestSeedIfEmptyAndLegacyRebuildMarker(t *testing.T) {
	ctx := context.Background()
	empty := storagetest.NewMemoryStore()
	if err := listingindex.SeedIfEmpty(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if ready, err := listingindex.Ready(ctx, empty); err != nil || !ready {
		t.Fatalf("new bucket ready=%v,%v", ready, err)
	}
	legacy := storagetest.NewMemoryStore()
	if err := legacy.Put(ctx, "sessions/codex/old/metadata.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.SeedIfEmpty(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if ready, err := listingindex.Ready(ctx, legacy); err != nil || ready {
		t.Fatalf("legacy ready=%v,%v", ready, err)
	}
}

func TestConcurrentHintsAndSessionCleanup(t *testing.T) {
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hash := fmt.Sprintf("%064x", i+1)
			entry := listingindex.Entry{Key: fmt.Sprintf("%s%019d/codex/session/%s.json", listingindex.Prefix, uint64(7000000000000000000)+uint64(i), hash), MetadataKey: "sessions/codex/session/metadata.json", Hash: hash}
			if err := listingindex.Put(ctx, store, entry); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	before, err := store.List(ctx, listingindex.Prefix)
	if err != nil || len(before) != 20 {
		t.Fatalf("hints=%d,%v", len(before), err)
	}
	if err := listingindex.DeleteSession(ctx, store, "codex", "session"); err != nil {
		t.Fatal(err)
	}
	after, err := store.List(ctx, listingindex.Prefix)
	if err != nil || len(after) != 0 {
		t.Fatalf("remaining hints=%d,%v", len(after), err)
	}
	if _, err := store.Get(ctx, listingindex.ReadyKey); err != storage.ErrNotFound {
		t.Fatalf("unexpected ready object: %v", err)
	}
}

func TestDeleteSessionDoesNotFollowCorruptPointerToAnotherSession(t *testing.T) {
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	otherHash := fmt.Sprintf("%064x", 1)
	otherKey := fmt.Sprintf("%s%019d/codex/other/%s.json", listingindex.Prefix, uint64(7000000000000000000), otherHash)
	other := listingindex.Entry{Key: otherKey, MetadataKey: "sessions/codex/other/metadata.json", Hash: otherHash}
	if err := listingindex.Put(ctx, store, other); err != nil {
		t.Fatal(err)
	}
	corruptPointer := "listing/by-session/codex/target/" + otherHash
	if err := store.Put(ctx, corruptPointer, []byte(otherKey)); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.DeleteSession(ctx, store, "codex", "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, otherKey); err != nil {
		t.Fatalf("another session's hint was deleted: %v", err)
	}
	if _, err := store.Get(ctx, corruptPointer); err != storage.ErrNotFound {
		t.Fatalf("corrupt pointer was not removed: %v", err)
	}
}
