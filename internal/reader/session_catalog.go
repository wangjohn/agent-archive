package reader

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
	"modernc.org/sqlite"
)

// SearchSummary is a private projection of published metadata. It deliberately
// excludes source references, counts, payloads, history and transcripts.
type SearchSummary struct {
	SessionID, NativeSessionID, MachineID, ProjectID, ProjectName, RepoKey string
	Name, Title, Branch, ParentSessionID                                   string
	StartedAt, CapturedAt                                                  time.Time
	EndedAt                                                                *time.Time
	Harness                                                                archive.Harness
	Parser                                                                 archive.ParserInfo
	Origin                                                                 archive.SessionOrigin
	Replay                                                                 *archive.Replay
	Models                                                                 []archive.ModelSummary
	SkillsAvailable                                                        []archive.SkillSnapshot
	SkillsUsed                                                             []archive.SkillUse
	SkillDetection                                                         archive.SkillDetection
	CaptureGaps                                                            []archive.CaptureGap
	PullRequests                                                           []archive.PullRequestLink
	GitActivity                                                            []archive.GitEvent
}

// Metadata returns the search/display projection, never a complete body.
func (s SearchSummary) Metadata() archive.Metadata {
	return archive.Metadata{SessionID: s.SessionID, NativeSessionID: s.NativeSessionID, MachineID: s.MachineID, ProjectID: s.ProjectID, ProjectName: s.ProjectName, RepoKey: s.RepoKey, Name: s.Name, Title: s.Title, Branch: s.Branch, ParentSessionID: s.ParentSessionID, StartedAt: s.StartedAt, CapturedAt: s.CapturedAt, EndedAt: s.EndedAt, Harness: s.Harness, Parser: s.Parser, Origin: s.Origin, Replay: s.Replay, Models: s.Models, SkillsAvailable: s.SkillsAvailable, SkillsUsed: s.SkillsUsed, SkillDetection: s.SkillDetection, CaptureGaps: s.CaptureGaps, PullRequests: s.PullRequests, GitActivity: s.GitActivity}
}

func summarize(m archive.Metadata) SearchSummary {
	// Retain only PR numbers, never repositories, URLs or unrelated git events.
	prs := make([]archive.PullRequestLink, 0, len(m.PullRequests))
	for _, pr := range m.PullRequests {
		prs = append(prs, archive.PullRequestLink{Number: pr.Number})
	}
	events := []archive.GitEvent{}
	for _, event := range m.GitActivity {
		if event.Kind == archive.GitEventPRCreated {
			events = append(events, archive.GitEvent{Kind: event.Kind, PRNumber: event.PRNumber})
		}
	}
	gaps := make([]archive.CaptureGap, len(m.CaptureGaps))
	for i, gap := range m.CaptureGaps {
		gaps[i] = archive.CaptureGap{Code: gap.Code}
	}
	var replay *archive.Replay
	if m.Replay != nil {
		replay = &archive.Replay{}
	}
	return SearchSummary{SessionID: m.SessionID, NativeSessionID: m.NativeSessionID, MachineID: m.MachineID, ProjectID: m.ProjectID, ProjectName: m.ProjectName, RepoKey: m.RepoKey, Name: m.Name, Title: m.Title, Branch: m.Branch, ParentSessionID: m.ParentSessionID, StartedAt: m.StartedAt, CapturedAt: m.CapturedAt, EndedAt: m.EndedAt, Harness: m.Harness, Parser: m.Parser, Origin: m.Origin, Replay: replay, Models: m.Models, SkillsAvailable: m.SkillsAvailable, SkillsUsed: m.SkillsUsed, SkillDetection: m.SkillDetection, CaptureGaps: gaps, PullRequests: prs, GitActivity: events}
}

// SessionCatalog indexes summaries after a fresh complete canonical discovery.
type SessionCatalog interface {
	Refresh(context.Context, HeaderSnapshot) error
	Query(context.Context, CatalogQuery) (CatalogPage, error)
}

// CatalogQuery selects a safe summary candidate set. Words bind the cursor;
// callers apply their text matcher before limiting final matches.
type CatalogQuery struct {
	Metadata MetadataQuery
	// Words bind the cursor but do not prune candidates: CLI Unicode, exact ID,
	// PR and configured project label rules remain the final authority.
	Words  []string
	Cursor string
}

// CatalogRow identifies one exact canonical metadata revision.
type CatalogRow struct {
	Key, ETag, Hash string
	Summary         SearchSummary
}

// CatalogPage reports candidate rows and an exact candidate total. Complete
// describes discovery and typed filtering, not caller-specific text matching.
type CatalogPage struct {
	Rows     []CatalogRow
	Next     string
	Total    int
	Complete bool
}

// ErrStaleCatalogCursor asks callers to restart their view after refresh.
var ErrStaleCatalogCursor = errors.New("sessions changed; refresh the view")

// SQLiteSessionCatalog retains summaries only; bodies stay in MetadataCache.
type SQLiteSessionCatalog struct {
	db    *sql.DB
	store storage.ObjectStore
	opts  ListOptions
}

// OpenSessionCatalog opens a disposable private SQLite index. Each connection
// has a busy timeout; a transaction serializes reconciliation across processes.
func OpenSessionCatalog(ctx context.Context, cache *MetadataCache, store storage.ObjectStore, opts ListOptions) (*SQLiteSessionCatalog, error) {
	return openSessionCatalog(ctx, cache, store, opts, false)
}

func openSessionCatalog(ctx context.Context, cache *MetadataCache, store storage.ObjectStore, opts ListOptions, repair bool) (*SQLiteSessionCatalog, error) {
	if cache == nil {
		return nil, errors.New("session catalog requires a metadata cache")
	}
	dir := filepath.Join(filepath.Dir(cache.dir), "catalog")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("catalog requires a real directory")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	if !repair {
		unlock, e := local.NamedLock(dir, "open.lock")
		if e != nil {
			return nil, e
		}
		defer unlock()
	}
	path := filepath.Join(dir, "sessions.sqlite")
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if info, e := os.Lstat(candidate); e == nil && !info.Mode().IsRegular() {
			return nil, errors.New("catalog requires regular files")
		} else if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("catalog requires a regular file")
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS catalog_state (id INTEGER PRIMARY KEY CHECK(id=1), generation INTEGER NOT NULL, epoch TEXT NOT NULL); CREATE TABLE IF NOT EXISTS sessions (key TEXT PRIMARY KEY, etag TEXT NOT NULL, hash TEXT NOT NULL, capture TEXT NOT NULL, activity TEXT NOT NULL, summary BLOB NOT NULL)`)
	if err == nil {
		_, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO catalog_state VALUES(1,0,?)", rand.Text())
	}
	if err != nil {
		db.Close()
		var damaged *sqlite.Error
		if !repair && errors.As(err, &damaged) && (damaged.Code() == 11 || damaged.Code() == 26) {
			// SQLite rejected this disposable file before any transaction. Retire
			// the damaged bytes and create a fresh summary index on this request.
			for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
				if e := os.Remove(candidate); e != nil && !os.IsNotExist(e) {
					return nil, e
				}
			}
			return openSessionCatalog(ctx, cache, store, opts, true)
		}
		return nil, err
	}
	opts.Cache = cache
	return &SQLiteSessionCatalog{db: db, store: store, opts: opts}, nil
}

// Close releases the catalog database handle.
func (c *SQLiteSessionCatalog) Close() error { return c.db.Close() }

// DiscoverCatalogHeaders always discovers the full canonical scope so a
// narrowed query cannot evict other projects or harnesses from the local index.
func DiscoverCatalogHeaders(ctx context.Context, store storage.ObjectStore, opts ListOptions) (HeaderSnapshot, error) {
	opts.Cache.maintain(ctx, 64)
	known := opts.Cache.keys("sessions/")
	objects, err := listObjects(ctx, store, "sessions/", known)
	if err != nil {
		return HeaderSnapshot{}, err
	}
	opts.Cache.evictUnlisted(known, objects)
	return HeaderSnapshot{Canonical: objects, knownCanonical: known}, nil
}

// Refresh atomically reconciles changed/deleted canonical revisions.
func (c *SQLiteSessionCatalog) Refresh(ctx context.Context, snapshot HeaderSnapshot) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire the SQLite writer before reading validators, preventing a stale
	// comparison from overwriting another connection's completed refresh.
	if _, err = tx.ExecContext(ctx, "UPDATE catalog_state SET generation=generation WHERE id=1"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT key,etag FROM sessions")
	if err != nil {
		return err
	}
	prior := map[string]string{}
	for rows.Next() {
		var k, e string
		if err = rows.Scan(&k, &e); err != nil {
			rows.Close()
			return err
		}
		prior[k] = e
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var pending []storage.Object
	for _, obj := range snapshot.Canonical {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !isMetadataKey(obj.Key) {
			continue
		}
		seen[obj.Key] = true
		if obj.ETag == "" || prior[obj.Key] != obj.ETag {
			pending = append(pending, obj)
		}
	}
	loaded, err := c.readChanges(ctx, pending, snapshot.Revisions)
	if err != nil {
		return err
	}
	changed := len(loaded) > 0
	for _, row := range loaded {
		data, e := json.Marshal(row.Summary)
		if e != nil {
			return e
		}
		m := row.Summary.Metadata()
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO sessions VALUES(?,?,?,?,?,?)", row.Key, row.ETag, row.Hash, catalogTime(m.CapturedAt), catalogTime(listingindex.ActivityTime(m)), data); err != nil {
			return err
		}
	}
	for key := range prior {
		if !seen[key] {
			if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE key=?", key); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		if _, err = tx.ExecContext(ctx, "UPDATE catalog_state SET generation=generation+1 WHERE id=1"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// readChanges overlaps independent cold/changed reads while keeping observers
// and errors in canonical selection order. Every worker joins before rollback.
func (c *SQLiteSessionCatalog) readChanges(ctx context.Context, objects []storage.Object, revisions map[RevisionID]listingindex.Revision) ([]CatalogRow, error) {
	results := make([]CatalogRow, len(objects))
	errs := make([]error, len(objects))
	cached := make([]bool, len(objects))
	next := 0
	completed := make(chan struct{}, len(objects))
	failed := false
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(listConcurrency, len(objects)) {
		wg.Go(func() {
			for {
				mu.Lock()
				if failed || ctx.Err() != nil || next == len(objects) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				var expected *listingindex.Revision
				if revision, ok := revisions[RevisionID{Key: objects[i].Key, ETag: objects[i].ETag}]; ok {
					expected = &revision
				}
				m, hash, hit, err := c.readRevision(ctx, objects[i], expected)
				cached[i] = hit
				errs[i] = err
				completed <- struct{}{}
				if err != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
					return
				}
				results[i] = CatalogRow{Key: objects[i].Key, ETag: objects[i].ETag, Hash: hash, Summary: summarize(m)}
			}
		})
	}
	go func() { wg.Wait(); close(completed) }()
	finished := 0
	for range completed {
		finished++
		if c.opts.Progress != nil {
			c.opts.Progress(finished, len(objects))
		}
	}
	for i := range next {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if c.opts.BodyRead != nil {
			c.opts.BodyRead(objects[i].Key, cached[i])
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Fixed precision preserves time ordering under SQLite text comparison.
func catalogTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func (c *SQLiteSessionCatalog) readRevision(ctx context.Context, obj storage.Object, expected *listingindex.Revision) (archive.Metadata, string, bool, error) {
	if obj.ETag == "" {
		return archive.Metadata{}, "", false, errors.New("catalog requires metadata revision validators")
	}
	data, cached := c.opts.Cache.get(obj.Key, obj.ETag)
	verifiable, valid := etagMatchesBytes(obj.ETag, data)
	if cached && (!verifiable || !valid) {
		cached = false
	}
	if !cached {
		var err error
		if getter, ok := c.store.(storage.VersionedGetter); ok {
			var etag string
			data, etag, err = getter.GetVersioned(ctx, obj.Key)
			if err == nil && etag != obj.ETag {
				return archive.Metadata{}, "", false, ErrRefreshRequired
			}
		} else {
			if !verifiable {
				return archive.Metadata{}, "", false, errors.New("store cannot verify catalog metadata revisions")
			}
			data, err = c.store.Get(ctx, obj.Key)
		}
		if err != nil {
			return archive.Metadata{}, "", false, err
		}
	}
	if verifiable, valid := etagMatchesBytes(obj.ETag, data); verifiable && !valid {
		return archive.Metadata{}, "", cached, ErrRefreshRequired
	}
	if expected != nil {
		if storage.SHA256Hex(data) != expected.Hash {
			return archive.Metadata{}, "", cached, ErrRefreshRequired
		}
		if err := expected.ValidateMetadata(data); err != nil {
			return archive.Metadata{}, "", cached, err
		}
	}
	m, err := decodeMetadata(obj.Key, data)
	if err != nil {
		return archive.Metadata{}, "", cached, err
	}
	if !cached {
		c.opts.Cache.putVerified(obj.Key, obj.ETag, data)
	}
	return m, storage.SHA256Hex(data), cached, nil
}

// Query returns summary candidates from one database generation.
func (c *SQLiteSessionCatalog) Query(ctx context.Context, q CatalogQuery) (CatalogPage, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CatalogPage{}, err
	}
	defer tx.Rollback()
	var generation int64
	var epoch string
	if err = tx.QueryRowContext(ctx, "SELECT generation,epoch FROM catalog_state WHERE id=1").Scan(&generation, &epoch); err != nil {
		return CatalogPage{}, err
	}
	binding, _ := json.Marshal(struct {
		Metadata MetadataQuery
		Words    []string
	}{q.Metadata, q.Words})
	digest := sha256.Sum256(binding)
	token := epoch + ":" + strconv.FormatInt(generation, 10) + ":" + hex.EncodeToString(digest[:]) + ":"
	offset := 0
	if q.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || !strings.HasPrefix(string(raw), token) {
			return CatalogPage{}, ErrStaleCatalogCursor
		}
		offset, e = strconv.Atoi(strings.TrimPrefix(string(raw), token))
		if e != nil || offset < 0 {
			return CatalogPage{}, ErrStaleCatalogCursor
		}
	}
	order := "capture DESC,key"
	if q.Metadata.Order == ActivityOrder {
		order = "activity DESC,capture DESC,key"
	}
	// SQL predicates are exact for these typed fields. Text stays a candidate
	// superset; Go's Unicode matcher and query-time labels decide final matches.
	where, args := catalogWhere(q.Metadata)
	complex := q.Metadata.Filter.Model != "" || q.Metadata.Filter.Skill != "" || q.Metadata.Filter.SkillSHA256 != "" || q.Metadata.Filter.RequireCompleteCoverage
	page := CatalogPage{Complete: true}
	statement := "SELECT key,etag,hash,summary FROM sessions" + where + " ORDER BY " + order
	if !complex {
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions"+where, args...).Scan(&page.Total); err != nil {
			return CatalogPage{}, err
		}
		if offset > page.Total {
			return CatalogPage{}, ErrStaleCatalogCursor
		}
		limit := q.Metadata.Limit
		if limit <= 0 {
			limit = -1
		}
		statement += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return CatalogPage{}, err
	}
	defer rows.Close()
	limit := q.Metadata.Limit
	for rows.Next() {
		var row CatalogRow
		var data []byte
		if err = rows.Scan(&row.Key, &row.ETag, &row.Hash, &data); err != nil {
			return CatalogPage{}, err
		}
		if err = json.Unmarshal(data, &row.Summary); err != nil {
			return CatalogPage{}, fmt.Errorf("invalid session catalog: %w", err)
		}
		m := row.Summary.Metadata()
		if !matches(m, q.Metadata.Filter) || q.Metadata.TopLevelOnly && m.ParentSessionID != "" {
			continue
		}
		if complex {
			page.Total++
		}
		if !complex || page.Total > offset && (limit <= 0 || len(page.Rows) < limit) {
			page.Rows = append(page.Rows, row)
		}
	}
	if err = rows.Err(); err != nil {
		return CatalogPage{}, err
	}
	if offset > page.Total {
		return CatalogPage{}, ErrStaleCatalogCursor
	}
	if offset+len(page.Rows) < page.Total {
		page.Next = base64.RawURLEncoding.EncodeToString([]byte(token + strconv.Itoa(offset+len(page.Rows))))
	}
	if err = rows.Close(); err != nil {
		return CatalogPage{}, err
	}
	return page, tx.Commit()
}

func catalogWhere(q MetadataQuery) (string, []any) {
	clauses := []string{"1=1"}
	var args []any
	add := func(clause string, value any) { clauses = append(clauses, clause); args = append(args, value) }
	f := q.Filter
	if f.Harness != "" {
		add("json_extract(summary,'$.Harness.name') = ?", f.Harness)
	}
	if !f.From.IsZero() {
		add("capture >= ?", catalogTime(f.From))
	}
	if !f.To.IsZero() {
		add("capture <= ?", catalogTime(f.To))
	}
	if q.TopLevelOnly {
		clauses = append(clauses, "json_extract(summary,'$.ParentSessionID') = ''")
	}
	if f.Replays == ReplaysHidden {
		clauses = append(clauses, "json_type(summary,'$.Replay') = 'null'")
	}
	if f.Replays == ReplaysOnly {
		clauses = append(clauses, "json_type(summary,'$.Replay') = 'object'")
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}
