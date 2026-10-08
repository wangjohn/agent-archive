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
	ScopeEmpty   bool
	Outside      int
	TotalMatched int
	Complete     bool
}

// ListRecent proves coverage from fresh canonical headers before choosing bodies.
// Unsupported predicates and incomplete indexes use the exhaustive cache reader.
func ListRecent(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, limit int, opts ListOptions) (RecentResult, error) {
	opts.Cache.maintain(ctx, 64)
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
	hints, err := listRevisionHeaders(ctx, store)
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
		if prior, exists := revisions[key]; exists && !prior.SameSummary(r) {
			return fallback("listing index has conflicting revision summaries")
		}
		revisions[key] = r
	}
	selected, result, err := selectListingRevisions(objects, revisions, filter, limit, opts)
	if err != nil {
		return fallback(err.Error())
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
		if err := r.ValidateMetadata(data); err != nil {
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
	allChildren := make(map[string]int)
	for _, m := range all {
		if m.ParentSessionID != "" {
			allChildren[m.Harness.Name+"/"+m.ParentSessionID]++
		}
	}
	scopeEmpty, outside := false, 0
	if opts.ScopeMatch != nil {
		scoped := make([]archive.Metadata, 0, len(all))
		for _, m := range all {
			if opts.ScopeMatch(m) {
				scoped = append(scoped, m)
			}
		}
		countScoped := len(scoped)
		if opts.TopLevelOnly {
			countScoped = 0
			for _, m := range scoped {
				if m.ParentSessionID == "" {
					countScoped++
				}
			}
		}
		outside = len(all) - len(scoped)
		if countScoped == 0 {
			scopeEmpty = true
		} else {
			all = scoped
		}
	}
	result := RecentResult{Complete: true, Children: allChildren, ScopeEmpty: scopeEmpty, Outside: outside}
	for _, m := range all {
		if m.ParentSessionID != "" {
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
// Each invocation resumes through convergent auxiliary writes; no ready marker is used.
func RebuildIndex(ctx context.Context, store storage.ObjectStore, prefix string) (int, error) {
	getter, ok := store.(storage.VersionedGetter)
	if !ok {
		return 0, errors.New("store cannot return metadata revision validators")
	}
	legacy, err := listingindex.LegacyEntries(ctx, store)
	if err != nil {
		return 0, err
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
		if err = listingindex.RebuildRevision(ctx, store, r, legacy[r.MetadataKey]); err != nil {
			return count, err
		}
		count++
	}
	if err := cleanupRevisionHeaders(ctx, store, legacy); err != nil {
		return count, err
	}
	return count, nil
}

// cleanupRevisionHeaders bounds explicit orphan/malformed cleanup. Repeating a
// partial rebuild resumes from the remaining immutable headers.
func cleanupRevisionHeaders(ctx context.Context, store storage.ObjectStore, legacy map[string][]storage.Object) error {
	hints, err := listRevisionHeaders(ctx, store)
	if err != nil {
		return err
	}
	bySession := make(map[string][]storage.Object)
	cleaned := 0
	for _, hint := range hints {
		r, parseErr := listingindex.ParseRevision(hint.Key)
		if parseErr == nil {
			bySession[r.MetadataKey] = append(bySession[r.MetadataKey], hint)
			continue
		}
		if cleaned == 32 {
			return errors.New("listing cleanup remains pending; rerun rebuild")
		}
		if err := store.Delete(ctx, hint.Key); err != nil {
			return err
		}
		cleaned++
	}
	for key, objects := range legacy {
		for _, obj := range objects {
			if strings.HasPrefix(obj.Key, "listing/by-session-v2/") {
				bySession[key] = append(bySession[key], obj)
			}
		}
	}
	statter, ok := store.(storage.ObjectStatter)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(bySession))
	for key := range bySession {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, err := statter.Stat(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			if err := listingindex.RetireSnapshot(ctx, store, key, bySession[key]); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

// selectListingRevisions proves canonical coverage and applies summary predicates before body reads.
func selectListingRevisions(objects []storage.Object, revisions map[string]listingindex.Revision, filter Filter, limit int, opts ListOptions) ([]listingindex.Revision, RecentResult, error) {
	var selected, all []listingindex.Revision
	allChildren := make(map[string]int)
	scopedHidden, allHidden := 0, 0
	for _, obj := range objects {
		if !isMetadataKey(obj.Key) {
			continue
		}
		r, exists := revisions[obj.Key+"\x00"+obj.ETag]
		if obj.ETag == "" || !exists {
			return nil, RecentResult{}, errors.New("listing index does not cover current metadata; run list --rebuild-index")
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
		parts := strings.Split(r.MetadataKey, "/")
		inScope := opts.ScopeMatch == nil || opts.ScopeMatch(archive.Metadata{ProjectID: r.ProjectID, RepoKey: r.RepoKey})
		if r.Parent != "" {
			allChildren[parts[1]+"/"+r.Parent]++
			if opts.TopLevelOnly {
				allHidden++
				if inScope {
					scopedHidden++
				}
				continue
			}
		}
		all = append(all, r)
		if inScope {
			selected = append(selected, r)
		}
	}
	scopeEmpty := opts.ScopeMatch != nil && len(selected) == 0
	hidden := scopedHidden
	outside := len(all) - len(selected)
	if scopeEmpty {
		selected = all
		hidden = allHidden
	}
	result := RecentResult{Complete: true, Children: allChildren, Hidden: hidden, Outside: outside, ScopeEmpty: scopeEmpty, TotalMatched: len(selected)}
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
	if len(selected) > limit {
		selected = selected[:limit]
	}
	return selected, result, nil
}

// listRevisionHeaders reads discovery summaries without downloading hint bodies.
func listRevisionHeaders(ctx context.Context, store storage.ObjectStore) ([]storage.Object, error) {
	v2, err := store.List(ctx, listingindex.V2Prefix)
	if err != nil {
		return nil, err
	}
	v3, err := store.List(ctx, listingindex.V3Prefix)
	if err != nil {
		return nil, err
	}
	return append(v2, v3...), nil
}
