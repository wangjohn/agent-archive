package cli

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
	"io"
)

// catalogSessions supplies the existing CLI matcher with typed projections.
// Paging happens after the final matcher, so exact IDs, configured labels and
// scope fallback cannot be lost to a reader-side candidate limit.
func catalogSessions(env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string, words []string) ([]archive.Metadata, bool, error) {
	return catalogSessionsInContext(context.Background(), env, store, opts, stderr, command, words)
}

func catalogSessionsInContext(ctx context.Context, env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string, words []string) ([]archive.Metadata, bool, error) {
	cache := listCache(env, opts.noCache)
	if cache == nil {
		return nil, false, nil
	}
	span := trace.Start("list metadata")
	defer span.End()
	readOpts := reader.ListOptions{Cache: cache, Skipped: warnSkippedSidecar(stderr, command), BodyRead: catalogBodyObserver(env)}
	catalog, err := reader.OpenSessionCatalog(ctx, cache, store, readOpts)
	if err != nil {
		return nil, false, nil
	}
	defer func() { _ = catalog.Close() }()
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
			// A damaged disposable index must not hide current archive sessions.
			// Reuse this command's fresh headers for the verified legacy fallback.
			sessions, fallbackErr := reader.ListMetadataFromSnapshot(ctx, store, headers, opts.filter, readOpts)
			sortByActivity(sessions)
			return filterListOrigin(sessions, opts.imported, opts.hookCaptured), true, fallbackErr
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

// readListCandidates retains selected reads for simple bounded listings and
// uses the summary catalog where exhaustive metadata used to be necessary.
func readListCandidates(env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, limit int, readOpts reader.ListOptions, full bool, stderr io.Writer) (reader.RecentResult, bool, error) {
	needsCatalog := opts.filter.Model != "" || opts.filter.Skill != "" || opts.filter.SkillSHA256 != "" || opts.filter.RequireCompleteCoverage
	if !opts.jsonOut && (full || needsCatalog) {
		// Complete summaries preserve child hints, scope tiers and ambiguity.
		sessions, used, err := catalogSessions(env, store, opts, stderr, "list", nil)
		if used {
			return reader.RecentResult{Sessions: sessions, Complete: true, TotalMatched: len(sessions)}, true, err
		}
	}
	listed, err := reader.ListRecent(context.Background(), store, archiveSessionsPrefix, opts.filter, limit, readOpts)
	return listed, full, err
}

// readShowCandidates returns search projections only. The resolver returns an
// identity, and its caller reads the selected full metadata before rendering.
func readShowCandidates(ctx context.Context, store storage.ObjectStore, env metadataCacheDependencies, harness, query string, stderr io.Writer) ([]archive.Metadata, error) {
	opts := listOptions{filter: reader.Filter{Harness: harness}}
	sessions, used, err := catalogSessionsInContext(ctx, env, store, opts, stderr, "show", nil)
	if used {
		return sessions, err
	}
	readOpts := reader.ListOptions{Cache: listCache(env, false), Skipped: warnSkippedSidecar(stderr, "show"), BodyRead: catalogBodyObserver(env)}
	return reader.FindMetadataPrefix(ctx, store, archiveSessionsPrefix, query, opts.filter, readOpts, func(archive.Metadata) bool { return true })
}

func catalogBodyObserver(env metadataCacheDependencies) func(string, bool) {
	if observer, ok := env.(interface{ listBodyObserver() func(string, bool) }); ok {
		return observer.listBodyObserver()
	}
	return nil
}

type catalogBrowseCommand string

const (
	catalogBrowseShow catalogBrowseCommand = "show"
	catalogBrowseList catalogBrowseCommand = "list"
)
