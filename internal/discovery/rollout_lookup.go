package discovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const maxCurrentThreads = 64

// CodexRolloutLookup is one serial collector-pass view of validated native facts.
// Neither the SQLite current locator nor a cached identity grants admission.
// Observation-cache membership never implies complete filesystem coverage.
type CodexRolloutLookup struct {
	store        *state.Store
	roots        []string
	deadline     time.Time
	observations map[string]rolloutObservation
	threads      map[string]agentapi.CodexRolloutSet
	indexes      map[string]*currentIndexView
	probes       int
	closed       bool
}

type rolloutObservation struct {
	source   SourceDescriptor
	stamp    Fingerprint
	identity codexmeta.CodexIdentity
}
type currentIndexView struct {
	snapshot    *privateIndex
	db          *sql.DB
	refreshed   bool
	unavailable bool
	stamps      []os.FileInfo
}

// NewCodexRolloutLookup composes existing catalog and registrations lazily with
// targeted current-row evidence. It does no native enumeration or SQL at setup.
func NewCodexRolloutLookup(ctx context.Context, store *state.Store, homes []string) (*CodexRolloutLookup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots := approvedRoots(homes)
	if len(roots) > 1024 {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("native home limit"))
	}
	lookup := &CodexRolloutLookup{store: store, roots: roots, deadline: time.Now().Add(Budget), observations: map[string]rolloutObservation{}, threads: map[string]agentapi.CodexRolloutSet{}, indexes: map[string]*currentIndexView{}}
	var prior catalog
	if err := local.Read(filepath.Join(store.Home(), "discovery-catalog.json"), &prior); err != nil && !errors.Is(err, os.ErrNotExist) {
		return lookup, nil
	}
	if catalogNeedsReset(prior, roots) {
		return lookup, nil
	}
	for path, entry := range prior.Cache {
		root := lookup.homeFor(path)
		if root == "" || entry.Observation.Identity == nil {
			continue
		}
		lookup.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: entry.Observation.Identity.RolloutID}, Fingerprint{Size: entry.Size, Mtime: entry.Mtime}, entry.Observation.Identity)
	}
	return lookup, nil
}

// Observe accepts already validated pass-owned metadata facts. It deep-copies
// them so caller mutations cannot silently alter a lookup revision token.
func (l *CodexRolloutLookup) Observe(source SourceDescriptor, stamp Fingerprint, identity *codexmeta.CodexIdentity) {
	if l.closed || identity == nil || identity.ThreadID == "" || identity.RolloutID == "" || source.Kind != archive.SourceKindFile || l.homeFor(source.Locator) != source.Root {
		return
	}
	if _, present := l.observations[source.Locator]; !present && len(l.observations) >= maxCatalog {
		return
	}
	raw, err := json.Marshal(identity)
	if err != nil || len(raw) > 4096 {
		return
	}
	var copyID codexmeta.CodexIdentity
	if json.Unmarshal(raw, &copyID) != nil {
		return
	}
	l.observations[source.Locator] = rolloutObservation{source: source, stamp: stamp, identity: copyID}
}

func (l *CodexRolloutLookup) homeFor(path string) string {
	if !filepath.IsAbs(path) || len(path) > 4096 || filepath.Clean(path) != path {
		return ""
	}
	for _, root := range l.roots {
		if local.PathWithin(path, filepath.Join(root, "sessions")) || local.PathWithin(path, filepath.Join(root, "archived_sessions")) {
			return root
		}
	}
	return ""
}

func (l *CodexRolloutLookup) checkBudget(ctx context.Context) error {
	if l.closed {
		return agentapi.Wrap(agentapi.Unavailable, agentapi.ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if time.Now().After(l.deadline) {
		return agentapi.Wrap(agentapi.Limit, errors.New("native lookup pass deadline"))
	}
	return nil
}

func (l *CodexRolloutLookup) inspect(ctx context.Context, path, thread string) (*agentapi.SourceRef, error) {
	root := l.homeFor(path)
	if root == "" {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("current locator outside native home"))
	}
	if l.probes >= HeaderProbes {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("native header probe limit"))
	}
	l.probes++
	header := sourcefacts.ReadHeader(ctx, root, path)
	if header.Identity == nil {
		return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("current header unavailable"))
	}
	if header.Identity.ThreadID != thread {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("current thread mismatch"))
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, agentapi.Wrap(agentapi.Changed, err)
	}
	l.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: header.Identity.RolloutID}, Fingerprint{Size: info.Size(), Mtime: info.ModTime().UnixNano()}, header.Identity)
	return &agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: thread}, nil
}

// Rollout returns known physical locators, revalidated by the source provider.
func (l *CodexRolloutLookup) Rollout(ctx context.Context, id string) ([]agentapi.SourceRef, error) {
	if err := l.checkBudget(ctx); err != nil {
		return nil, err
	}
	var refs []agentapi.SourceRef
	for _, observation := range l.observations {
		if observation.identity.RolloutID == id {
			refs = append(refs, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: observation.source.Locator, Key: observation.identity.ThreadID})
		}
	}
	if len(refs) > archive.MaxHistorySpans {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
	}
	slices.SortFunc(refs, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	return refs, nil
}

// Thread supplies current facts, plus bounded known same-thread candidates.
func (l *CodexRolloutLookup) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	if err := l.checkBudget(ctx); err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	if codexmeta.RolloutID(id+".jsonl") != id || id == "" {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unsafe, errors.New("invalid native thread"))
	}
	if len(l.candidateDigests(id)) > archive.MaxHistorySpans {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Limit, errors.New("thread candidate limit"))
	}
	if cached, present := l.threads[id]; present {
		return l.withCandidates(id, cached), nil
	}
	if len(l.threads) >= maxCurrentThreads {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Limit, errors.New("current thread query limit"))
	}
	l.registrationHint(ctx, id)
	var current *agentapi.SourceRef
	for _, root := range l.roots {
		view, err := l.index(ctx, root, false)
		if err != nil {
			continue
		}
		path, found, err := currentLocator(ctx, view.db, id)
		if err != nil || !found {
			continue
		}
		if l.homeFor(path) != root {
			return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unsafe, errors.New("current locator belongs to another native home"))
		}
		ref, err := l.inspect(ctx, path, id)
		if err != nil {
			return agentapi.CodexRolloutSet{}, err
		}
		if current != nil && current.Path != ref.Path {
			return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unavailable, errors.New("conflicting current locators"))
		}
		current = ref
	}
	if len(l.candidateDigests(id)) > archive.MaxHistorySpans {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Limit, errors.New("thread candidate limit"))
	}
	set := l.withCandidates(id, agentapi.CodexRolloutSet{Current: current})
	l.threads[id] = set
	return set, nil
}

func (l *CodexRolloutLookup) registrationHint(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	key, err := agentmeta.NewSessionKey("codex", id)
	if err != nil {
		return
	}
	archiveID, found, err := l.store.ArchiveSessionID(key)
	if err != nil || !found {
		return
	}
	reg, found, err := l.store.LoadRegistration(archiveID)
	if err != nil || !found {
		return
	}
	if l.homeFor(reg.TranscriptPath) != "" {
		_, _ = l.inspect(ctx, reg.TranscriptPath, id)
	}
}

func (l *CodexRolloutLookup) withCandidates(id string, set agentapi.CodexRolloutSet) agentapi.CodexRolloutSet {
	set.Candidates = nil
	for _, o := range l.observations {
		if o.identity.ThreadID == id {
			set.Candidates = append(set.Candidates, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: o.source.Locator, Key: id})
		}
	}
	slices.SortFunc(set.Candidates, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	// Complete deliberately remains false until independent catalog coverage exists.
	set.Complete = false
	data, _ := json.Marshal(struct {
		Current      *agentapi.SourceRef
		Observations []rolloutObservationDigest
	}{Current: set.Current, Observations: l.candidateDigests(id)})
	sum := sha256.Sum256(data)
	set.Revision = hex.EncodeToString(sum[:])
	return set
}

type rolloutObservationDigest struct {
	Path     string
	Stamp    Fingerprint
	Identity codexmeta.CodexIdentity
}

func (l *CodexRolloutLookup) candidateDigests(id string) []rolloutObservationDigest {
	var out []rolloutObservationDigest
	for _, o := range l.observations {
		if o.identity.ThreadID == id {
			out = append(out, rolloutObservationDigest{o.source.Locator, o.stamp, o.identity})
		}
	}
	slices.SortFunc(out, func(a, b rolloutObservationDigest) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Check revalidates selection within the pass. One private refresh per home is
// allowed; subsequent changes remain pending instead of making unbounded copies.
func (l *CodexRolloutLookup) Check(ctx context.Context, id, revision string) error {
	if err := l.checkBudget(ctx); err != nil {
		return err
	}
	old, found := l.threads[id]
	if !found {
		return agentapi.Wrap(agentapi.Changed, errIndexChanged)
	}
	var current *agentapi.SourceRef
	for _, root := range l.roots {
		view, err := l.index(ctx, root, true)
		if err != nil {
			if old.Current != nil && l.homeFor(old.Current.Path) == root {
				return agentapi.Wrap(agentapi.Changed, err)
			}
			continue
		}
		path, found, err := currentLocator(ctx, view.db, id)
		if err != nil {
			return agentapi.Wrap(agentapi.Changed, err)
		}
		if !found {
			continue
		}
		// The source provider checks opened header/content separately. Requerying
		// the same locator does not repeat its native header scan.
		ref := &agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id}
		if l.homeFor(path) != root {
			return agentapi.Wrap(agentapi.Unsafe, errIndexChanged)
		}
		if current != nil && current.Path != ref.Path {
			return agentapi.Wrap(agentapi.Changed, errIndexChanged)
		}
		current = ref
	}
	if len(l.candidateDigests(id)) > archive.MaxHistorySpans {
		return agentapi.Wrap(agentapi.Limit, errors.New("thread candidate limit"))
	}
	now := l.withCandidates(id, agentapi.CodexRolloutSet{Current: current})
	if now.Revision != revision {
		return agentapi.Wrap(agentapi.Changed, errIndexChanged)
	}
	return nil
}

func indexStamps(root string) []os.FileInfo {
	var out []os.FileInfo
	for _, suffix := range []string{"", "-wal"} {
		info, _ := os.Lstat(filepath.Join(root, "state_5.sqlite") + suffix)
		out = append(out, info)
	}
	return out
}
func equalIndexStamps(a, b []os.FileInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil || b[i] == nil {
			if a[i] != nil || b[i] != nil {
				return false
			}
			continue
		}
		if !os.SameFile(a[i], b[i]) || a[i].Size() != b[i].Size() || !a[i].ModTime().Equal(b[i].ModTime()) {
			return false
		}
	}
	return true
}

func (l *CodexRolloutLookup) index(ctx context.Context, root string, refresh bool) (*currentIndexView, error) {
	view := l.indexes[root]
	if view != nil {
		if !refresh || equalIndexStamps(view.stamps, indexStamps(root)) {
			if view.unavailable {
				return nil, errIndexChanged
			}
			return view, nil
		}
		if view.refreshed {
			return nil, errIndexChanged
		}
		if view.db != nil {
			_ = view.db.Close()
			_ = view.snapshot.close()
		}
		view = &currentIndexView{refreshed: true}
		l.indexes[root] = view
	} else {
		view = &currentIndexView{}
		l.indexes[root] = view
	}
	ctx, cancel := context.WithDeadline(ctx, l.deadline)
	defer cancel()
	capturedStamps := indexStamps(root)
	snapshot, err := snapshotCurrentIndex(ctx, root, nil)
	if err != nil {
		view.unavailable = true
		view.stamps = indexStamps(root)
		return nil, err
	}
	params := url.Values{"mode": {"ro"}, "immutable": {"1"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: snapshot.path, RawQuery: params.Encode()}).String())
	if err != nil {
		_ = snapshot.close()
		view.unavailable = true
		return nil, err
	}
	db.SetMaxOpenConns(1)
	view.db = db
	view.snapshot = snapshot
	view.stamps = capturedStamps
	return view, nil
}

func currentLocator(ctx context.Context, db *sql.DB, id string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, indexHintTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = conn.Close() }()
	for _, limit := range []struct{ id, value int }{{sqlite3.SQLITE_LIMIT_LENGTH, 1 << 20}, {sqlite3.SQLITE_LIMIT_SQL_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_ATTACHED, 0}, {sqlite3.SQLITE_LIMIT_VDBE_OP, 20000}} {
		if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
			return "", false, err
		}
	}
	const query = "SELECT substr(rollout_path,1,4097) FROM threads WHERE id=? LIMIT 2"
	plan, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, id)
	if err != nil {
		return "", false, err
	}
	valid := false
	count := 0
	for plan.Next() {
		var a, b, c int
		var detail string
		if err := plan.Scan(&a, &b, &c, &detail); err != nil {
			_ = plan.Close()
			return "", false, err
		}
		count++
		if count > 16 || len(detail) > 2048 || !strings.Contains(detail, "SEARCH threads USING INDEX sqlite_autoindex_threads_1 (id=?)") {
			_ = plan.Close()
			return "", false, errors.New("unsupported current primary key")
		}
		valid = true
	}
	err = plan.Err()
	_ = plan.Close()
	if err != nil || !valid {
		return "", false, errors.New("current primary key unavailable")
	}
	rows, err := conn.QueryContext(ctx, query, id)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	var path string
	found := false
	for rows.Next() {
		if found {
			return "", false, errors.New("duplicate current thread")
		}
		if err := rows.Scan(&path); err != nil {
			return "", false, err
		}
		if path == "" || len(path) > 4096 {
			return "", false, errors.New("current locator length")
		}
		found = true
	}
	return path, found, rows.Err()
}

// Close releases all private databases and snapshots; it never changes natives.
func (l *CodexRolloutLookup) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	var errs []error
	for _, view := range l.indexes {
		if view.db != nil {
			errs = append(errs, view.db.Close())
		}
		if view.snapshot != nil {
			errs = append(errs, view.snapshot.close())
		}
	}
	return errors.Join(errs...)
}

var _ agentapi.CodexRolloutLookup = (*CodexRolloutLookup)(nil)
