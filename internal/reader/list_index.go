package reader

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// RecentResult is a bounded newest-first view. Complete means every matching identity was considered, so TotalMatched
// is exact. Indexed completeness proves discovery coverage, not validation
// of unselected bodies.
type RecentResult struct {
	Sessions     []archive.Metadata
	Hidden       int
	Children     map[string]int
	TotalMatched int
	Complete     bool
}

// ListRecent proves coverage from fresh canonical headers before choosing bodies.
// Unsupported predicates and incomplete indexes use the exhaustive cache reader.
func ListRecent(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, limit int, opts ListOptions) (RecentResult, error) {
	fallback := func(reason string) (RecentResult, error) {
		if opts.CompatibilityScan != nil {
			opts.CompatibilityScan(reason)
		}
		return listRecentFull(ctx, store, prefix, filter, limit, opts)
	}
	getter, ok := store.(storage.VersionedGetter)
	if !ok || limit <= 0 || filter.Model != "" || filter.Skill != "" || filter.SkillSHA256 != "" || filter.RequireCompleteCoverage {
		return fallback("query requires an exhaustive metadata scan")
	}
	objects, err := store.List(ctx, listPrefixFor(prefix, filter.Harness))
	if err != nil {
		return RecentResult{}, err
	}
	if opts.Cache != nil {
		opts.Cache.evictUnlisted(opts.Cache.keys(listPrefixFor(prefix, filter.Harness)), objects)
	}
	hints, err := store.List(ctx, listingindex.V2Prefix)
	if err != nil {
		return RecentResult{}, err
	}
	revisions := make(map[string]listingindex.Revision)
	for _, hint := range hints {
		r, err := listingindex.ParseRevision(hint.Key)
		if err != nil {
			return fallback("listing index contains an unsupported or damaged entry; run list --rebuild-index")
		}
		key := r.MetadataKey + "\x00" + r.ETag
		if prior, exists := revisions[key]; exists && prior.Key != r.Key {
			return fallback("listing index has conflicting revision summaries")
		}
		revisions[key] = r
	}
	var selected []listingindex.Revision
	result := RecentResult{Complete: true, Children: make(map[string]int)}
	for _, obj := range objects {
		if !isMetadataKey(obj.Key) {
			continue
		}
		r, exists := revisions[obj.Key+"\x00"+obj.ETag]
		if obj.ETag == "" || !exists {
			return fallback("listing index does not cover current metadata; run list --rebuild-index")
		}
		if filter.Harness != "" && !strings.HasPrefix(r.MetadataKey, "sessions/"+filter.Harness+"/") {
			continue
		}
		if !filter.From.IsZero() && r.CapturedAt.Before(filter.From) || !filter.To.IsZero() && r.CapturedAt.After(filter.To) {
			continue
		}
		if filter.Replays == ReplaysHidden && r.Replay || filter.Replays == ReplaysOnly && !r.Replay {
			continue
		}
		if r.Parent != "" {
			parts := strings.Split(r.MetadataKey, "/")
			result.Children[parts[1]+"/"+r.Parent]++
			if opts.TopLevelOnly {
				result.Hidden++
				continue
			}
		}
		selected = append(selected, r)
	}
	sort.Slice(selected, func(i, j int) bool {
		a, b := selected[i], selected[j]
		if opts.ActivityOrder && !a.Activity.Equal(b.Activity) {
			return a.Activity.After(b.Activity)
		}
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		return a.MetadataKey < b.MetadataKey
	})
	result.TotalMatched = len(selected)
	if len(selected) > limit {
		selected = selected[:limit]
	}
	for _, r := range selected {
		data, cached := opts.Cache.get(r.MetadataKey, r.ETag)
		if cached && storage.SHA256Hex(data) != r.Hash {
			cached = false
		}
		if !cached {
			var validator string
			data, validator, err = getter.GetVersioned(ctx, r.MetadataKey)
			if err != nil {
				return RecentResult{}, fmt.Errorf("incomplete listing: selected metadata %q changed or cannot be read; retry or use --limit 0: %w", r.MetadataKey, err)
			}
			if validator != r.ETag || storage.SHA256Hex(data) != r.Hash {
				return RecentResult{}, fmt.Errorf("incomplete listing: metadata %q changed during query; retry or use --limit 0", r.MetadataKey)
			}
		}
		if opts.BodyRead != nil {
			opts.BodyRead(r.MetadataKey, cached)
		}
		metadata, err := decodeMetadata(r.MetadataKey, data)
		if err != nil {
			return RecentResult{}, err
		}
		check, err := listingindex.NewRevision(r.MetadataKey, data, r.ETag)
		if err != nil || check.Key != r.Key {
			return RecentResult{}, fmt.Errorf("incomplete listing: invalid revision summary for %q", r.MetadataKey)
		}
		if !cached {
			opts.Cache.putVerified(r.MetadataKey, r.ETag, data)
		}
		result.Sessions = append(result.Sessions, metadata)
	}
	return result, nil
}

func listRecentFull(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, limit int, opts ListOptions) (RecentResult, error) {
	all, err := ListMetadataWithOptions(ctx, store, prefix, filter, opts)
	if err != nil {
		return RecentResult{}, err
	}
	result := RecentResult{Complete: true, Children: make(map[string]int)}
	for _, m := range all {
		if m.ParentSessionID != "" {
			result.Children[m.Harness.Name+"/"+m.ParentSessionID]++
			if opts.TopLevelOnly {
				result.Hidden++
			}
		}
	}
	if opts.TopLevelOnly {
		top := all[:0]
		for _, m := range all {
			if m.ParentSessionID == "" {
				top = append(top, m)
			}
		}
		all = top
	}
	if opts.ActivityOrder {
		sort.SliceStable(all, func(i, j int) bool { return listingindex.ActivityTime(all[i]).After(listingindex.ActivityTime(all[j])) })
	}
	result.TotalMatched = len(all)
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	result.Sessions = all
	return result, nil
}

// RebuildIndex validates canonical bodies and writes only auxiliary revisions.
// Each invocation resumes by replaying idempotent writes; no ready marker is used.
func RebuildIndex(ctx context.Context, store storage.ObjectStore, prefix string) (int, error) {
	getter, ok := store.(storage.VersionedGetter)
	if !ok {
		return 0, errors.New("store cannot return metadata revision validators")
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, obj := range objects {
		if !isMetadataKey(obj.Key) {
			continue
		}
		data, etag, err := getter.GetVersioned(ctx, obj.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return count, err
		}
		r, err := listingindex.NewRevision(obj.Key, data, etag)
		if err != nil {
			return count, fmt.Errorf("rebuild index %q: %w", obj.Key, err)
		}
		if err = listingindex.RepairRevision(ctx, store, r); err != nil {
			return count, err
		}
		count++
	}
	// Remove malformed v2 entries only after successful canonical validation.
	hints, err := store.List(ctx, listingindex.V2Prefix)
	if err != nil {
		return count, err
	}
	for _, hint := range hints {
		r, parseErr := listingindex.ParseRevision(hint.Key)
		if parseErr != nil {
			if err = store.Delete(ctx, hint.Key); err != nil {
				return count, err
			}
			continue
		}
		if statter, ok := store.(storage.ObjectStatter); ok {
			if _, err = statter.Stat(ctx, r.MetadataKey); errors.Is(err, storage.ErrNotFound) {
				if err = listingindex.DeleteRevision(ctx, store, r); err != nil {
					return count, err
				}
			} else if err != nil {
				return count, err
			}
		}
	}
	return count, nil
}
