package reader

import (
	"encoding/base64"
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

// Existing readable single-component summaries are not subject to new write bounds.
func TestLegacyListingReadDoesNotRequirePortableReencoding(t *testing.T) {
	for _, scenario := range []string{"long_namespace", "separator_budget"} {
		t.Run(scenario, func(t *testing.T) {
			store := newOpaqueListingStore()
			id := "legacy"
			if scenario == "long_namespace" {
				id = strings.Repeat("x", 241)
			}
			key := putSession(t, store, "codex", id, baseTime)
			body, err := store.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			var m archive.Metadata
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			var historical string
			for size := range 1024 {
				if scenario == "separator_budget" {
					m.RepoKey = strings.Repeat("k", size)
				}
				body, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Put(t.Context(), key, body); err != nil {
					t.Fatal(err)
				}
				_, validator, err := store.GetVersioned(t.Context(), key)
				if err != nil {
					t.Fatal(err)
				}
				legacy, err := listingindex.New(key, body)
				if err != nil {
					t.Fatal(err)
				}
				r := listingindex.Revision{Nonce: strings.Repeat("A", 26), ETag: validator, Hash: legacy.Hash, Activity: listingindex.ActivityTime(m), ProjectID: m.ProjectID, RepoKey: m.RepoKey}
				encoded, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				parts := strings.Split(strings.TrimPrefix(legacy.Key, listingindex.Prefix), "/")
				historical = listingindex.V3Prefix + parts[1] + "/" + parts[2] + "/" + parts[0] + "/" + base64.RawURLEncoding.EncodeToString(encoded)
				if len(historical) > 1024 {
					t.Fatal("no readable legacy boundary fixture")
				}
				// Preserve the old nonce length to isolate the new component/separator bounds.
				if _, err := listingindex.NewRevision(key, body, validator); err != nil {
					break
				}
				historical = ""
			}
			if historical == "" {
				t.Fatal("legacy fixture must exceed new write bounds")
			}
			if _, err := listingindex.ParseRevision(historical); err != nil {
				t.Fatal(err)
			}
			if err := store.Put(t.Context(), historical, nil); err != nil {
				t.Fatal(err)
			}
			got, err := ListRecent(t.Context(), store, "sessions", Filter{}, 1, ListOptions{})
			if err != nil || !got.Complete || len(got.Sessions) != 1 || got.Sessions[0].SessionID != id || got.Sessions[0].RepoKey != m.RepoKey {
				t.Fatal("legacy canonical listing became unreadable", got, err)
			}
		})
	}
}
