package listingindex

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// LegacyEntries snapshots v2 hints and cleanup pointers for explicit migration.
// Ordinary v3 publication never enumerates these archive-wide namespaces.
func LegacyEntries(ctx context.Context, store storage.ObjectStore) (map[string][]storage.Object, error) {
	hints, err := store.List(ctx, V2Prefix)
	if err != nil {
		return nil, err
	}
	pointers, err := store.List(ctx, v2Pointers)
	if err != nil {
		return nil, err
	}
	bySession := make(map[string][]storage.Object)
	for _, obj := range append(hints, pointers...) {
		var key string
		if strings.HasPrefix(obj.Key, V2Prefix) {
			r, err := ParseRevision(obj.Key)
			if err != nil {
				continue
			}
			key = r.MetadataKey
		} else {
			parts := strings.Split(strings.TrimPrefix(obj.Key, v2Pointers), "/")
			if len(parts) != 3 {
				continue
			}
			key, err = archive.MetadataObjectKey(parts[0], parts[1])
			if err != nil {
				continue
			}
		}
		bySession[key] = append(bySession[key], obj)
	}
	return bySession, nil
}

// RebuildRevision migrates a frozen legacy snapshot while publishing a fresh
// v3 survivor. The combined v2/v3 retirement budget is 32 candidates.
func RebuildRevision(ctx context.Context, store storage.ObjectStore, r Revision, legacy []storage.Object) error {
	return repairRevision(ctx, store, r, legacy)
}

func retireCandidates(ctx context.Context, store storage.ObjectStore, metadataKey string, objects []storage.Object) error {
	pointerPrefix := revisionPointerPrefix(Revision{MetadataKey: metadataKey})
	known := make(map[string]bool)
	var candidates []storage.Object
	for _, obj := range objects {
		if strings.HasPrefix(obj.Key, v2Pointers) {
			continue
		}
		r, err := ParseRevision(obj.Key)
		if err != nil {
			continue
		}
		if r.MetadataKey != metadataKey {
			continue
		}
		candidates = append(candidates, obj)
		known[revisionPointer(r)] = true
	}
	for _, obj := range objects {
		if strings.HasPrefix(obj.Key, pointerPrefix) && !known[obj.Key] {
			candidates = append(candidates, obj)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Key < candidates[j].Key })
	for i, obj := range candidates {
		if i == 32 {
			return errors.New("listing cleanup remains pending")
		}
		if err := retireCandidate(ctx, store, metadataKey, obj); err != nil {
			return err
		}
	}
	return nil
}

func retireCandidate(ctx context.Context, store storage.ObjectStore, metadataKey string, obj storage.Object) error {
	if !strings.HasPrefix(obj.Key, v2Pointers) {
		r, err := ParseRevision(obj.Key)
		if err != nil || r.MetadataKey != metadataKey {
			return errors.New("invalid listing cleanup identity")
		}
		return DeleteRevision(ctx, store, r)
	}
	data, err := store.Get(ctx, obj.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	r, parseErr := ParseRevision(string(data))
	// A corrupt pointer never authorizes deleting its claimed target. Its own
	// session-qualified auxiliary object can still be removed safely. Legacy
	// pointers can own only v2 hints; v3 hints have no pointer counterpart.
	if parseErr == nil && strings.HasPrefix(r.Key, V2Prefix) && r.MetadataKey == metadataKey && revisionPointer(r) == obj.Key {
		return DeleteRevision(ctx, store, r)
	}
	return store.Delete(ctx, obj.Key)
}

func deleteRevisions(ctx context.Context, store storage.ObjectStore, harness, id string) error {
	key, err := archive.MetadataObjectKey(harness, id)
	if err != nil {
		return err
	}
	candidates, err := store.List(ctx, revisionSessionPrefix(key))
	if err != nil {
		return err
	}
	// Legacy time-addressed hints need an archive-wide header sweep to find
	// pointerless artifacts. No metadata, source or hint body is downloaded.
	hints, err := store.List(ctx, V2Prefix)
	if err != nil {
		return err
	}
	pointers, err := store.List(ctx, revisionPointerPrefix(Revision{MetadataKey: key}))
	if err != nil {
		return err
	}
	candidates = append(candidates, hints...)
	candidates = append(candidates, pointers...)
	return retireCandidates(ctx, store, key, candidates)
}

// RetireSnapshot removes at most 32 validated auxiliary candidates for one
// canonical identity. Callers freeze it before fresh publication or confirm
// canonical absence; legacy pointer pairs may require two DELETEs per candidate.
func RetireSnapshot(ctx context.Context, store storage.ObjectStore, metadataKey string, objects []storage.Object) error {
	parts := strings.Split(metadataKey, "/")
	if len(parts) != 4 {
		return errors.New("invalid listing cleanup identity")
	}
	expected, err := archive.MetadataObjectKey(parts[1], parts[2])
	if err != nil || expected != metadataKey {
		return errors.New("invalid listing cleanup identity")
	}
	return retireCandidates(ctx, store, metadataKey, objects)
}
