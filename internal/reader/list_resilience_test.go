package reader

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// deleteAfterList deletes one sidecar right after the listing, as another
// Mac's retention or undo can at any time.
type deleteAfterList struct {
	*countingStore
	key string
}

func (d deleteAfterList) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	objects, err := d.countingStore.List(ctx, prefix)
	_ = d.MemoryStore.Delete(ctx, d.key)
	return objects, err
}

// A sidecar deleted between the listing and its read no longer exists: it is
// left out, and the rest are listed.
func TestListSkipsSidecarDeletedAfterListing(t *testing.T) {
	store := newCountingStore()
	var keys []string
	for i := 0; i < 5; i++ {
		keys = append(keys, putSession(t, store, "codex", fmt.Sprintf("s%02d", i), baseTime))
	}
	var skipped []SkippedSidecar
	results, err := ListMetadataWithOptions(context.Background(), deleteAfterList{store, keys[2]}, "sessions", Filter{}, ListOptions{Skipped: func(s SkippedSidecar) { skipped = append(skipped, s) }})
	if err != nil {
		t.Fatalf("a deleted sidecar failed the listing: %v", err)
	}
	if len(results) != 4 || len(skipped) != 0 {
		t.Fatalf("results=%d skipped=%v", len(results), skipped)
	}
	for _, m := range results {
		if m.SessionID == "s02" {
			t.Fatalf("deleted session listed")
		}
	}
}

// A sidecar that does not validate (damaged, or written by a newer version)
// is left out and reported, in key order, instead of failing the listing.
func TestListReportsInvalidSidecarsAndListsTheRest(t *testing.T) {
	store := newCountingStore()
	var keys []string
	for i := 0; i < 6; i++ {
		keys = append(keys, putSession(t, store, "codex", fmt.Sprintf("s%02d", i), baseTime))
	}
	for _, k := range []string{keys[4], keys[1]} {
		if err := store.Put(context.Background(), k, []byte(`{"schema_version":99}`)); err != nil {
			t.Fatal(err)
		}
	}
	var skipped []SkippedSidecar
	results, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, ListOptions{Skipped: func(s SkippedSidecar) { skipped = append(skipped, s) }})
	if err != nil {
		t.Fatalf("an invalid sidecar failed the listing: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d", len(results))
	}
	if len(skipped) != 2 || skipped[0].Key != keys[1] || skipped[1].Key != keys[4] || !errors.Is(skipped[0].Err, ErrInvalidMetadata) {
		t.Fatalf("skipped = %+v", skipped)
	}
	// Without a callback the listing still succeeds.
	if results, err := ListMetadata(context.Background(), store, "sessions", Filter{}); err != nil || len(results) != 4 {
		t.Fatalf("results=%d err=%v", len(results), err)
	}
}
