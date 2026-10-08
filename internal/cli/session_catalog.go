package cli

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"io"
)

// catalogSessions supplies the existing CLI matcher with typed projections.
// Paging happens after the final matcher, so exact IDs, configured labels and
// scope fallback cannot be lost to a reader-side candidate limit.
func catalogSessions(env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string, words []string) ([]archive.Metadata, bool, error) {
	cache := listCache(env, opts.noCache)
	if cache == nil {
		return nil, false, nil
	}
	ctx := context.Background()
	readOpts := reader.ListOptions{Cache: cache, Skipped: warnSkippedSidecar(stderr, command)}
	if observer, ok := env.(interface{ listBodyObserver() func(string, bool) }); ok {
		readOpts.BodyRead = observer.listBodyObserver()
	}
	catalog, err := reader.OpenSessionCatalog(ctx, cache, store, readOpts)
	if err != nil {
		return nil, false, nil
	}
	defer catalog.Close()
	headers, err := reader.DiscoverCatalogHeaders(ctx, store, readOpts)
	if err != nil {
		return nil, true, err
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		sessions, fallbackErr := reader.ListMetadataFromSnapshot(ctx, store, headers, opts.filter, readOpts)
		sortByActivity(sessions)
		return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, fallbackErr
	}
	query := reader.CatalogQuery{Words: words, Metadata: reader.MetadataQuery{Filter: opts.filter, Order: reader.ActivityOrder}}
	var sessions []archive.Metadata
	for {
		page, e := catalog.Query(ctx, query)
		if e != nil {
			return nil, true, e
		}
		for _, row := range page.Rows {
			sessions = append(sessions, row.Summary.Metadata())
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, nil
}
