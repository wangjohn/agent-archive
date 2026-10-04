package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type opaqueListingStore struct {
	*indexedCountingStore
	change func(string)
}

func (s *opaqueListingStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	objects, err := s.indexedCountingStore.List(ctx, prefix)
	for i := range objects {
		objects[i].ETag = "opaque/revision:" + objects[i].ETag
	}
	return objects, err
}
func (s *opaqueListingStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.MemoryStore.Stat(ctx, key)
	info.ETag = "opaque/revision:" + info.ETag
	return info, err
}
func (s *opaqueListingStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	if s.change != nil {
		s.change(key)
	}
	data, etag, err := s.indexedCountingStore.GetVersioned(ctx, key)
	return data, "opaque/revision:" + etag, err
}
func newOpaqueListingStore() *opaqueListingStore {
	return &opaqueListingStore{indexedCountingStore: &indexedCountingStore{countingStore: newCountingStore()}}
}

// The body budget counts local cache opens as well as remote responses. Doubling
// archive size changes header work, never the selected-body work.
func TestListRevisionBodyBudgetColdWarmAndExhaustiveOracle(t *testing.T) {
	t.Parallel()
	for _, size := range []int{10000, 20000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := newOpaqueListingStore()
			for i := range size {
				// Timestamp ties and backfills deliberately disagree with identity order.
				key := putSession(t, store, "codex", fmt.Sprintf("%032x", i+1), baseTime.Add(time.Duration(i%701)*time.Second))
				data, etag, err := store.GetVersioned(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				revision, err := listingindex.NewRevision(key, data, etag)
				if err != nil {
					t.Fatal(err)
				}
				if err = listingindex.PutRevision(ctx, store, revision); err != nil {
					t.Fatal(err)
				}
			}
			oracle, err := ListMetadataWithOptions(ctx, store, "sessions", Filter{}, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cache, err := OpenMetadataCache(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, warm := range []bool{false, true} {
				store.reset()
				bodies, cached := 0, 0
				result, err := ListRecent(ctx, store, "sessions", Filter{}, 50, ListOptions{Cache: cache, BodyRead: func(_ string, hit bool) {
					bodies++
					if hit {
						cached++
					}
				}})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result.Sessions, oracle[:50]) || !result.Complete || result.TotalMatched != size {
					t.Fatal("indexed results disagree with exhaustive oracle")
				}
				_, gets := store.counts()
				wantGets := 50
				wantCached := 0
				if warm {
					wantGets = 0
					wantCached = 50
				}
				if bodies != 50 || cached != wantCached || len(gets) != wantGets {
					t.Fatalf("warm=%v bodies=%d cached=%d GETs=%d", warm, bodies, cached, len(gets))
				}
				for _, key := range gets {
					if !isMetadataKey(key) {
						t.Fatalf("list downloaded source or index body: %s", key)
					}
				}
			}
		})
	}
}

func TestListRevisionCoverageDetectsMixedWritersAndStaleEntries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newOpaqueListingStore()
	first := putSession(t, store, "codex", "first", baseTime)
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", "old-writer", baseTime.Add(time.Hour))
	scans := 0
	result, err := ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scans++ }})
	if err != nil || scans != 1 || result.Sessions[0].SessionID != "old-writer" || result.TotalMatched != 2 {
		t.Fatalf("mixed writer omitted: scans=%d err=%v", scans, err)
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	putSession(t, store, "codex", "first", baseTime.Add(2*time.Hour))
	result, err = ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{})
	if err != nil || result.Sessions[0].SessionID != "first" {
		t.Fatal("stale revision concealed rewrite")
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	store.change = func(key string) {
		store.change = nil
		if key == first {
			putSession(t, store, "codex", "first", baseTime.Add(3*time.Hour))
		}
	}
	if _, err = ListRecent(ctx, store, "sessions", Filter{}, 1, ListOptions{}); err == nil || !strings.Contains(err.Error(), "incomplete listing") {
		t.Fatalf("concurrent rewrite accepted: %v", err)
	}
}

func TestListRevisionActivityAndChildSummariesMatchExhaustive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newOpaqueListingStore()
	for i := range 7 {
		key := putSession(t, store, "codex", fmt.Sprintf("s%d", i), baseTime.Add(time.Duration(i)*time.Hour))
		data, err := store.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		var m archive.Metadata
		if err = json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			m.ParentSessionID = "s3"
		}
		if i == 3 {
			ended := baseTime.Add(10 * time.Hour)
			m.EndedAt = &ended
		}
		data, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := RebuildIndex(ctx, store, "sessions"); err != nil {
		t.Fatal(err)
	}
	opts := ListOptions{ActivityOrder: true, TopLevelOnly: true}
	bounded, err := ListRecent(ctx, store, "sessions", Filter{}, 2, opts)
	if err != nil {
		t.Fatal(err)
	}
	full, err := ListRecent(ctx, store, "sessions", Filter{}, 0, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bounded.Sessions, full.Sessions[:2]) || bounded.TotalMatched != 4 || bounded.Hidden != 3 || bounded.Children["codex/s3"] != 3 {
		t.Fatalf("summary parity failed: total=%d hidden=%d children=%v", bounded.TotalMatched, bounded.Hidden, bounded.Children)
	}
}
