package reader

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
)

func TestPortableListingChunksAndMalformedMixedHintsKeepCanonicalCoverage(t *testing.T) {
	store := newOpaqueListingStore()
	key := putSession(t, store, "codex", "portable", baseTime)
	body, err := store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.RepoKey = strings.Repeat("synthetic-repository", 12)
	body, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), key, body); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(t.Context(), store, key, body); err != nil {
		t.Fatal(err)
	}
	hints, err := store.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 1 || !strings.Contains(hints[0].Key, "/c/") {
		t.Fatal("long summary fixture lacks portable hint", hints, err)
	}
	scan := false
	got, err := ListRecent(t.Context(), store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scan = true }})
	if err != nil || scan || !got.Complete || got.TotalMatched != 1 || len(got.Sessions) != 1 || got.Sessions[0].RepoKey != metadata.RepoKey {
		t.Fatal("portable indexed coverage lost", got, err)
	}
	// The old single-component parser rejects this shape. Without a supported
	// hint, canonical header coverage cannot be asserted: the old reader's
	// existing fallback remains necessary, never a transparent cleanup claim.
	objects, err := store.List(t.Context(), "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectListingRevisions(objects, map[string]listingindex.Revision{}, Filter{}, 1, ListOptions{}); err == nil {
		t.Fatal("unsupported hints falsely establish coverage")
	}
	if err := store.Put(t.Context(), hints[0].Key+"/", nil); err != nil {
		t.Fatal(err)
	}
	scan = false
	got, err = ListRecent(t.Context(), store, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scan = true }})
	if err != nil || !scan || !got.Complete || got.TotalMatched != 1 || len(got.Sessions) != 1 || got.Sessions[0].RepoKey != metadata.RepoKey {
		t.Fatal("malformed mixed hints bypassed canonical fallback", got, err)
	}
}
