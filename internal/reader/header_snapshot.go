package reader

import (
	"context"
	"sync"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// RevisionID identifies a summary for an exact canonical object revision.
type RevisionID struct {
	Key  string
	ETag string
}

// HeaderSnapshot holds fresh headers for one query. Canonical is a complete
// listing, including objects without summaries; it is never cached as existence
// authority for another query.
type HeaderSnapshot struct {
	Canonical []storage.Object
	Revisions map[RevisionID]listingindex.Revision

	incompleteReason string
	knownCanonical   []string
}

// discoverHeaders overlaps independent canonical and auxiliary listings. Only
// canonical cache keys plan ranges; v2 must remain global for legacy coverage.
func discoverHeaders(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, cache *MetadataCache) (HeaderSnapshot, error) {
	canonicalPrefix := listPrefixFor(prefix, filter.Harness)
	known := cache.keys(canonicalPrefix)
	groups, err := listHeaderGroups(ctx, store, canonicalPrefix, known, true, listPrefixFor(listingindex.V3Prefix, filter.Harness))
	if err != nil {
		return HeaderSnapshot{}, err
	}
	snapshot := headerSnapshotFromGroups(groups)
	snapshot.knownCanonical = known
	if cache != nil {
		cache.evictUnlisted(known, snapshot.Canonical)
	}
	return snapshot, nil
}

// headerSnapshotFromGroups preserves complete canonical headers even when
// auxiliary summaries cannot safely select metadata bodies.
func headerSnapshotFromGroups(groups [3][]storage.Object) HeaderSnapshot {
	snapshot := HeaderSnapshot{Canonical: groups[0], Revisions: make(map[RevisionID]listingindex.Revision)}
hints:
	for _, group := range groups[1:] {
		for _, hint := range group {
			r, err := listingindex.ParseRevision(hint.Key)
			if err != nil {
				snapshot.incompleteReason = "listing index contains an unsupported or damaged entry; run list --rebuild-index"
				break hints
			}
			id := RevisionID{Key: r.MetadataKey, ETag: r.ETag}
			if prior, exists := snapshot.Revisions[id]; exists && !prior.SameSummary(r) {
				snapshot.incompleteReason = "listing index has conflicting revision summaries"
				break hints
			}
			snapshot.Revisions[id] = r
		}
	}
	return snapshot
}

// listHeaderGroups joins all started listings before returning. The first
// provider error cancels siblings and remains the returned error even when
// those siblings subsequently report cancellation.
func listHeaderGroups(ctx context.Context, store storage.ObjectStore, canonicalPrefix string, known []string, includeCanonical bool, v3Prefix string) ([3][]storage.Object, error) {
	caller := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var groups [3][]storage.Object
	var wg sync.WaitGroup
	var failed sync.Once
	var failure error
	for i, prefix := range []string{canonicalPrefix, listingindex.V2Prefix, v3Prefix} {
		if i == 0 && !includeCanonical {
			continue
		}
		wg.Go(func() {
			var objects []storage.Object
			var err error
			if i == 0 {
				objects, err = listObjects(ctx, store, prefix, known)
			} else {
				objects, err = store.List(ctx, prefix)
			}
			if err != nil {
				failed.Do(func() {
					failure = err
					cancel()
				})
				return
			}
			groups[i] = objects
		})
	}
	wg.Wait()
	if failure != nil {
		return groups, failure
	}
	return groups, caller.Err()
}
