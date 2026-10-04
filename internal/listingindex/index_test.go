package listingindex_test

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// putLegacyHint models a v1 writer so cleanup remains backwards compatible.
func putLegacyHint(ctx context.Context, store storage.ObjectStore, entry listingindex.Entry) error {
	prefix, err := listingindex.SessionPrefix(path.Base(path.Dir(path.Dir(entry.MetadataKey))), path.Base(path.Dir(entry.MetadataKey)))
	if err != nil {
		return err
	}
	if err := store.Put(ctx, prefix+entry.Hash, []byte(entry.Key)); err != nil {
		return err
	}
	return store.Put(ctx, entry.Key, []byte(entry.MetadataKey))
}

func TestConcurrentHintsAndSessionCleanup(t *testing.T) {
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			hash := fmt.Sprintf("%064x", i+1)
			entry := listingindex.Entry{Key: fmt.Sprintf("%s%019d/codex/session/%s.json", listingindex.Prefix, uint64(7000000000000000000)+uint64(i), hash), MetadataKey: "sessions/codex/session/metadata.json", Hash: hash}
			if err := putLegacyHint(ctx, store, entry); err != nil {
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
	if _, err := store.Get(ctx, "listing/v1-ready"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unexpected ready object: %v", err)
	}
}

func TestDeleteSessionDoesNotFollowCorruptPointerToAnotherSession(t *testing.T) {
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	otherHash := fmt.Sprintf("%064x", 1)
	otherKey := fmt.Sprintf("%s%019d/codex/other/%s.json", listingindex.Prefix, uint64(7000000000000000000), otherHash)
	other := listingindex.Entry{Key: otherKey, MetadataKey: "sessions/codex/other/metadata.json", Hash: otherHash}
	if err := putLegacyHint(ctx, store, other); err != nil {
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
	if _, err := store.Get(ctx, corruptPointer); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("corrupt pointer was not removed: %v", err)
	}
}
