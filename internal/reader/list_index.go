package reader

import (
	"context"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// RecentResult is a bounded newest-first view. Complete means every matching
// sidecar was considered, so TotalMatched is exact. When Complete is false,
// there are more matches but their total is intentionally unknown.
type RecentResult struct {
	Sessions     []archive.Metadata
	TotalMatched int
	Complete     bool
}

// ListRecent uses a completed immutable index when the store supports pages.
// Old buckets and stores without paging use the authoritative full scan.
func ListRecent(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, limit int, opts ListOptions) (RecentResult, error) {
	pager, canPage := store.(storage.PageLister)
	if !canPage || limit == 0 {
		return listRecentFull(ctx, store, prefix, filter, limit, opts)
	}
	ready, err := listingindex.Ready(ctx, store)
	if err != nil {
		return RecentResult{}, err
	}
	if !ready {
		return listRecentFull(ctx, store, prefix, filter, limit, opts)
	}
	result := RecentResult{Complete: true}
	seen := make(map[string]struct{})
	continuation := ""
	for {
		page, err := pager.ListPage(ctx, listingindex.Prefix, continuation, 128)
		if err != nil {
			return RecentResult{}, err
		}
		for _, obj := range page.Objects {
			entry, err := listingindex.Parse(obj.Key)
			if err != nil {
				// A damaged hint could conceal a live session. Use the
				// authoritative full scan until the index is rebuilt.
				return listRecentFull(ctx, store, prefix, filter, limit, opts)
			}
			if !filter.From.IsZero() && entry.CapturedAt.Before(filter.From) {
				result.TotalMatched = len(result.Sessions)
				return result, nil
			}
			data, err := store.Get(ctx, entry.MetadataKey)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return RecentResult{}, fmt.Errorf("read indexed metadata %q: %w", entry.MetadataKey, err)
			}
			if storage.SHA256Hex(data) != entry.Hash {
				continue
			}
			metadata, err := decodeMetadata(entry.MetadataKey, data)
			if errors.Is(err, ErrInvalidMetadata) {
				if opts.Skipped != nil {
					opts.Skipped(SkippedSidecar{Key: entry.MetadataKey, Err: err})
				}
				continue
			}
			if err != nil {
				return RecentResult{}, err
			}
			if !metadata.CapturedAt.Equal(entry.CapturedAt) {
				continue
			}
			if _, ok := seen[entry.MetadataKey]; ok {
				continue
			}
			seen[entry.MetadataKey] = struct{}{}
			if matches(metadata, filter) {
				result.Sessions = append(result.Sessions, metadata)
				if len(result.Sessions) > limit {
					result.Sessions = result.Sessions[:limit]
					result.Complete = false
					return result, nil
				}
			}
		}
		if page.Next == "" {
			break
		}
		continuation = page.Next
	}
	result.TotalMatched = len(result.Sessions)
	return result, nil
}

func listRecentFull(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, limit int, opts ListOptions) (RecentResult, error) {
	all, err := ListMetadataWithOptions(ctx, store, prefix, filter, opts)
	if err != nil {
		return RecentResult{}, err
	}
	total := len(all)
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return RecentResult{Sessions: all, TotalMatched: total, Complete: true}, nil
}

// RebuildIndex makes the index complete for an existing bucket. Publication
// writes hints before replacing metadata, so writers racing this full scan
// cannot create an unindexed live sidecar. MarkReady is written last.
func RebuildIndex(ctx context.Context, store storage.ObjectStore, prefix string) (int, error) {
	// A malformed hint forces every limited listing back to a full scan.
	// Remove only malformed keys here: valid hints may belong to writers
	// publishing concurrently and must remain available before their
	// sidecars replace the previous revisions.
	hints, err := store.List(ctx, listingindex.Prefix)
	if err != nil {
		return 0, err
	}
	for _, hint := range hints {
		if _, err := listingindex.Parse(hint.Key); err != nil {
			if err := store.Delete(ctx, hint.Key); err != nil {
				return 0, fmt.Errorf("remove malformed listing hint %q: %w", hint.Key, err)
			}
		}
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, object := range objects {
		if !isMetadataKey(object.Key) {
			continue
		}
		data, err := store.Get(ctx, object.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return count, err
		}
		entry, err := listingindex.New(object.Key, data)
		if err != nil {
			return count, fmt.Errorf("rebuild index %q: %w", object.Key, err)
		}
		if err := listingindex.Put(ctx, store, entry); err != nil {
			return count, err
		}
		count++
	}
	if err := listingindex.MarkReady(ctx, store); err != nil {
		return count, err
	}
	return count, nil
}
