package reader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// CatalogAuthority reports opt-in catalog metadata authority truthfully.
func CatalogAuthority(store storage.ObjectStore) bool {
	authority, ok := store.(interface{ CatalogMetadataAuthority() bool })
	return ok && authority.CatalogMetadataAuthority()
}

func catalogRows(ctx context.Context, snapshot *catalog.Snapshot) ([]catalog.Row, error) {
	var rows []catalog.Row
	cursor := ""
	for {
		page, err := snapshot.Query(ctx, catalog.Query{Index: catalog.IdentityIndex}, cursor, 1000)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Rows...)
		if page.Next == "" {
			return rows, nil
		}
		cursor = page.Next
	}
}

func hydrateCatalogRows(ctx context.Context, snapshot *catalog.Snapshot, rows []catalog.Row, opts ListOptions) ([]archive.Metadata, error) {
	span := trace.Start("read sidecars")
	span.Count("sidecars", len(rows))
	cacheHits, downloaded := 0, 0
	defer func() { span.Count("from cache", cacheHits); span.Count("downloaded", downloaded); span.End() }()
	result := make([]archive.Metadata, 0, len(rows))
	for i, row := range rows {
		raw, cached := opts.Cache.get(row.Key, row.Entry.Revision)
		if cached && !storage.VerifySHA256(raw, row.Entry.Metadata.SHA256) {
			cached = false
		}
		if !cached {
			var err error
			raw, err = snapshot.ReadMetadata(ctx, row.Entry)
			if err != nil {
				return nil, err
			}
		}
		metadata, err := decodeMetadata(row.Key, raw)
		if err != nil {
			return nil, err
		}
		canonical, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
		a, _ := json.Marshal(metadata)
		b, _ := json.Marshal(row.Entry.Summary)
		if err != nil || canonical != row.Key || string(a) != string(b) {
			return nil, errors.New("catalog selected body differs from summary")
		}
		if !cached {
			opts.Cache.putVerified(row.Key, row.Entry.Revision, raw)
		}
		if cached {
			cacheHits++
		} else {
			downloaded++
		}
		if opts.BodyRead != nil {
			opts.BodyRead(row.Key, cached)
		}
		if opts.Progress != nil {
			opts.Progress(i+1, len(rows))
		}
		result = append(result, metadata)
	}
	return result, ctx.Err()
}

func catalogMetadata(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, opts ListOptions) ([]archive.Metadata, error) {
	snapshot, err := catalog.OpenSnapshot(ctx, store, nil)
	if err != nil {
		return nil, err
	}
	rows, err := catalogRows(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	selected := rows[:0]
	for _, row := range rows {
		if strings.HasPrefix(row.Key, canonicalCatalogPrefix(prefix)) && matches(row.Entry.Summary, filter) {
			selected = append(selected, row)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		a, b := selected[i].Entry.Summary, selected[j].Entry.Summary
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		return selected[i].Key < selected[j].Key
	})
	return hydrateCatalogRows(ctx, snapshot, selected, opts)
}

func selectCatalogMetadata(ctx context.Context, store storage.ObjectStore, prefix string, q MetadataQuery, opts ListOptions) (RecentResult, error) {
	snapshot, err := catalog.OpenSnapshot(ctx, store, nil)
	if err != nil {
		return RecentResult{}, err
	}
	f := q.Filter
	// A single indexed predicate is bounded. Complex/scope filters retain the
	// complete summary universe and existing final matcher.
	if boundedCatalogSelection(prefix, q, opts) {
		return selectBoundedCatalog(ctx, snapshot, q, opts)
	}
	span := trace.Start("catalog summary fallback")
	defer span.End()
	rows, err := catalogRows(ctx, snapshot)
	if err != nil {
		return RecentResult{}, err
	}
	objects := make([]storage.Object, 0, len(rows))
	revisions := map[RevisionID]listingindex.Revision{}
	byKey := map[string]catalog.Row{}
	for _, row := range rows {
		if !strings.HasPrefix(row.Key, canonicalCatalogPrefix(prefix)) || !matches(row.Entry.Summary, f) {
			continue
		}
		m := row.Entry.Summary
		objects = append(objects, storage.Object{Key: row.Key, ETag: row.Entry.Revision})
		revisions[RevisionID{row.Key, row.Entry.Revision}] = listingindex.Revision{MetadataKey: row.Key, ETag: row.Entry.Revision, Hash: row.Entry.Metadata.SHA256, CapturedAt: m.CapturedAt, Activity: listingindex.ActivityTime(m), Parent: m.ParentSessionID, Replay: m.Replay != nil, ProjectID: m.ProjectID, RepoKey: m.RepoKey}
		byKey[row.Key] = row
	}
	selected, result, err := selectListingRevisions(objects, revisions, f, q.Limit, opts)
	if err != nil {
		return result, err
	}
	rows = nil
	for _, revision := range selected {
		rows = append(rows, byKey[revision.MetadataKey])
	}
	result.Sessions, err = hydrateCatalogRows(ctx, snapshot, rows, opts)
	return result, err
}

// CatalogSummaries discovers the complete fresh catalog summary universe.
// Complex CLI matchers and scope precedence may use this O(N) fallback without
// canonical LIST or metadata-body reads. It does not promise bounded work.
func CatalogSummaries(ctx context.Context, store storage.ObjectStore, filter Filter) ([]archive.Metadata, error) {
	snapshot, err := catalog.OpenSnapshot(ctx, store, nil)
	if err != nil {
		return nil, err
	}
	rows, err := catalogRows(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	result := make([]archive.Metadata, 0, len(rows))
	for _, row := range rows {
		if matches(row.Entry.Summary, filter) {
			result = append(result, summarize(row.Entry.Summary).Metadata())
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := result[i], result[j]
		at, bt := listingindex.ActivityTime(a), listingindex.ActivityTime(b)
		if !at.Equal(bt) {
			return at.After(bt)
		}
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		if a.Harness.Name != b.Harness.Name {
			return a.Harness.Name < b.Harness.Name
		}
		return a.SessionID < b.SessionID
	})
	return result, nil
}

// RefreshRemote reconciles one verified prior-root to new-root delta. Unknown
// roots rebuild the complete summary universe. It never asserts canonical LIST
// completeness for a subset of remote leaves.
func (c *SQLiteSessionCatalog) RefreshRemote(ctx context.Context) error {
	span := trace.Start("refresh session catalog")
	reused := 0
	defer func() { span.Count("from catalog", reused); span.End() }()
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	snapshot, err := catalog.OpenSnapshot(ctx, c.store, nil)
	if err != nil {
		return err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "UPDATE catalog_state SET generation=generation WHERE id=1"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS remote_root(id INTEGER PRIMARY KEY CHECK(id=1),root BLOB NOT NULL)"); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, "SELECT root FROM remote_root WHERE id=1").Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var prior catalog.ObjectRef
	if len(raw) > 0 && json.Unmarshal(raw, &prior) != nil {
		return errors.New("invalid remote catalog root")
	}
	delta, err := snapshot.Delta(ctx, prior)
	if err != nil {
		return err
	}
	// Validate the complete persisted tuple before reusing unchanged leaves.
	// Missing local rows require a complete verified summary rebuild as well.
	rows, err := tx.QueryContext(ctx, "SELECT key,etag,hash,capture,activity,summary,search,lowerid,unlabeled,summary_hash FROM sessions")
	if err != nil {
		return err
	}
	var invalid []string
	cachedKeys := map[string]bool{}
	for rows.Next() {
		var record catalogRecord
		if err = record.scan(rows); err != nil {
			_ = rows.Close()
			return err
		}
		cachedKeys[record.key] = true
		if !record.valid() {
			invalid = append(invalid, record.key)
		}
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	expected, err := snapshot.Count(ctx, catalog.Query{Index: catalog.CaptureIndex})
	if err != nil {
		return err
	}
	if !delta.Rebuild {
		for _, key := range invalid {
			entry, e := snapshot.Find(ctx, key)
			if e != nil {
				return e
			}
			if entry == nil {
				delta.Removed = append(delta.Removed, key)
			} else {
				delta.Changed = append(delta.Changed, catalog.Row{Key: key, Entry: *entry})
			}
		}
	}
	if !delta.Rebuild {
		for _, key := range delta.Removed {
			delete(cachedKeys, key)
		}
		for _, row := range delta.Changed {
			cachedKeys[row.Key] = true
		}
		if uint64(len(cachedKeys)) != expected {
			delta, err = snapshot.Delta(ctx, catalog.ObjectRef{})
			if err != nil {
				return err
			}
		}
	}
	if delta.Rebuild {
		if _, err = tx.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
			return err
		}
	}
	for _, key := range delta.Removed {
		if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE key=?", key); err != nil {
			return err
		}
	}
	for _, row := range delta.Changed {
		summary := summarize(row.Entry.Summary)
		data, e := json.Marshal(summary)
		if e != nil {
			return e
		}
		m := summary.Metadata()
		record := catalogRecord{key: row.Key, etag: row.Entry.Revision, hash: row.Entry.Metadata.SHA256, capture: catalogTime(m.CapturedAt), activity: catalogTime(listingindex.ActivityTime(m)), summary: data, search: catalogSearch(summary), lowerID: strings.ToLower(m.SessionID), unlabeled: m.ProjectName == ""}
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO sessions VALUES(?,?,?,?,?,?,?,?,?,?)", record.key, record.etag, record.hash, record.capture, record.activity, record.summary, record.search, record.lowerID, record.unlabeled, catalogRecordChecksum(record)); err != nil {
			return err
		}
	}
	raw, err = json.Marshal(delta.Next)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO remote_root VALUES(1,?)", raw); err != nil {
		return err
	}
	changed := delta.Rebuild || delta.Prior != delta.Next || len(invalid) > 0
	if changed {
		_, err = tx.ExecContext(ctx, "UPDATE catalog_state SET generation=generation+1,complete=1 WHERE id=1")
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE catalog_state SET complete=1 WHERE id=1")
	}
	if err != nil {
		return err
	}
	var epoch string
	var generation int64
	if err = tx.QueryRowContext(ctx, "SELECT epoch,generation FROM catalog_state WHERE id=1").Scan(&epoch, &generation); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if !delta.Rebuild {
		refreshed := map[string]bool{}
		for _, key := range invalid {
			refreshed[key] = true
		}
		for _, row := range delta.Changed {
			refreshed[row.Key] = true
		}
		for _, key := range delta.Removed {
			refreshed[key] = true
		}
		for key := range cachedKeys {
			if !refreshed[key] {
				reused++
			}
		}
	}
	c.remoteSnapshot = snapshot
	c.viewEpoch, c.viewGeneration, c.viewReady = epoch, generation, true
	return nil
}

func canonicalCatalogPrefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		prefix = "sessions"
	}
	return prefix + "/"
}

func boundedCatalogSelection(prefix string, q MetadataQuery, opts ListOptions) bool {
	f := q.Filter
	if canonicalCatalogPrefix(prefix) != "sessions/" || opts.ScopeMatch != nil || f.Harness != "" || f.Model != "" || f.Skill != "" || f.SkillSHA256 != "" || f.RequireCompleteCoverage {
		return false
	}
	replayRange := q.TopLevelOnly && (f.Replays == ReplaysHidden || f.Replays == ReplaysOnly) || !q.TopLevelOnly && f.Replays == ReplaysIncluded
	return replayRange && (!q.TopLevelOnly || f.From.IsZero() && f.To.IsZero()) && (q.Order == CaptureOrder || f.From.IsZero() && f.To.IsZero())
}

func catalogCaptureBounds(prefix string, f Filter) (string, string) {
	lower, upper := prefix+"0", prefix+":"
	if !f.From.IsZero() {
		lower = prefix + catalogTime(f.From)
	}
	if !f.To.IsZero() {
		upper = prefix + catalogTime(f.To) + "0"
	}
	return lower, upper
}

func selectBoundedCatalog(ctx context.Context, snapshot *catalog.Snapshot, q MetadataQuery, opts ListOptions) (RecentResult, error) {
	f := q.Filter
	idx := catalog.CaptureIndex
	if q.Order == ActivityOrder {
		idx = catalog.ActivityIndex
	}
	prefix := ""
	if q.TopLevelOnly {
		prefix = catalog.OrderPrefix(f.Replays == ReplaysOnly)
	}
	lower, upper := catalogCaptureBounds(prefix, f)
	query := catalog.Query{Index: idx, Lower: lower, Upper: upper, Reverse: true}
	total, err := snapshot.Count(ctx, query)
	if err != nil {
		return RecentResult{}, err
	}
	result := RecentResult{Complete: true, TotalMatched: int(total), Children: map[string]int{}}
	var rows []catalog.Row
	cursor := ""
	for len(rows) < q.Limit || q.Limit <= 0 {
		limit := 1000
		if q.Limit > 0 {
			limit = min(limit, q.Limit-len(rows))
		}
		page, e := snapshot.Query(ctx, query, cursor, limit)
		if e != nil {
			return RecentResult{}, e
		}
		rows = append(rows, page.Rows...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if q.TopLevelOnly {
		// Count children globally from the dedicated child discriminator range.
		// Parent count ranges are date ordered, and include exact harness+parent.
		// Global child count uses all order entries minus both root ranges when
		// replay filtering is absent; replay-filtered hidden counts use a summary
		// discriminator aggregate introduced in the capture/activity tree.
		hiddenQuery := query
		hiddenQuery.Lower = strings.Replace(lower, "!root/", "!children/", 1)
		hiddenQuery.Upper = strings.Replace(upper, "!root/", "!children/", 1)
		hidden, e := snapshot.Count(ctx, hiddenQuery)
		if e != nil {
			return RecentResult{}, e
		}
		result.Hidden = int(hidden)
		for _, row := range rows {
			m := row.Entry.Summary
			count := row.Entry.OrdinaryChildren
			if f.Replays == ReplaysOnly {
				count = row.Entry.ReplayChildren
			}
			if count > 0 {
				result.Children[m.Harness.Name+"/"+m.SessionID] = int(count)
			}
		}
	}
	result.Sessions, err = hydrateCatalogRows(ctx, snapshot, rows, opts)
	return result, err
}
