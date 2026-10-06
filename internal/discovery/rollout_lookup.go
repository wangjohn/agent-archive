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
	store         *state.Store
	readBudget    *agentapi.NativeReadBudget
	coverage      *coverageInventory
	coverageDirty bool
	roots         []string
	deadline      time.Time
	remaining     time.Duration
	observations  map[string]rolloutObservation
	byThread      map[string]map[string]struct{}
	byPhysical    map[string]map[string]struct{}
	threads       map[string]agentapi.CodexRolloutSet
	indexes       map[string]*currentIndexView
	probes        int
	closed        bool
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
	lookup := &CodexRolloutLookup{store: store, readBudget: agentapi.NewNativeReadBudget(currentSnapshotLimit), roots: roots, deadline: time.Now().Add(Budget), remaining: Budget, coverage: newCoverage(roots), observations: map[string]rolloutObservation{}, byThread: map[string]map[string]struct{}{}, byPhysical: map[string]map[string]struct{}{}, threads: map[string]agentapi.CodexRolloutSet{}, indexes: map[string]*currentIndexView{}}
	var prior catalog
	if err := local.Read(filepath.Join(store.Home(), "discovery-catalog.json"), &prior); err != nil && !errors.Is(err, os.ErrNotExist) {
		return lookup, nil
	}
	if prior.Coverage != nil && prior.Coverage.Version != 1 {
		return nil, errors.New("native coverage requires a newer writer")
	}
	if catalogNeedsReset(prior, roots) {
		return lookup, nil
	}
	if prior.Coverage != nil {
		if err := prior.Coverage.validate(roots); err != nil {
			return nil, err
		}
		lookup.coverage = prior.Coverage
	} else {
		lookup.coverage = newCoverage(roots)
	}
	for path, entry := range prior.Cache {
		root := lookup.homeFor(path)
		if root == "" || entry.Observation.Identity == nil {
			continue
		}
		lookup.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: entry.Observation.Identity.RolloutID}, Fingerprint{Size: entry.Size, Mtime: entry.Mtime}, entry.Observation.Identity)
	}
	lookup.coverageDirty = false
	return lookup, nil
}

// Observe accepts already validated pass-owned metadata facts. It deep-copies
// them so caller mutations cannot silently alter a lookup revision token.
func (l *CodexRolloutLookup) Observe(source SourceDescriptor, stamp Fingerprint, identity *codexmeta.CodexIdentity) {
	if l.closed || identity == nil || identity.ThreadID == "" || identity.RolloutID == "" || source.Kind != archive.SourceKindFile || l.homeFor(source.Locator) != source.Root {
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
	if l.coverage != nil {
		l.coverage.observe(source, stamp, copyID)
	}
	if prior, present := l.observations[source.Locator]; present {
		delete(l.byThread[prior.identity.ThreadID], source.Locator)
		delete(l.byPhysical[prior.identity.RolloutID], source.Locator)
	} else if len(l.observations) >= maxCatalog {
		return
	}
	l.observations[source.Locator] = rolloutObservation{source: source, stamp: stamp, identity: copyID}
	if l.byThread[copyID.ThreadID] == nil {
		l.byThread[copyID.ThreadID] = map[string]struct{}{}
	}
	l.byThread[copyID.ThreadID][source.Locator] = struct{}{}
	if l.byPhysical[copyID.RolloutID] == nil {
		l.byPhysical[copyID.RolloutID] = map[string]struct{}{}
	}
	l.byPhysical[copyID.RolloutID][source.Locator] = struct{}{}
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
	if l.remaining <= 0 {
		return agentapi.Wrap(agentapi.Limit, errors.New("native lookup pass deadline"))
	}
	return nil
}

func (l *CodexRolloutLookup) inspect(ctx context.Context, path, thread string) (*agentapi.SourceRef, error) {
	root := l.homeFor(path)
	if root == "" {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("current locator outside native home"))
	}
	if prior, present := l.observations[path]; present && prior.identity.ThreadID == thread {
		opened, err := sourcefacts.OpenRegular(root, path)
		if err == nil {
			info, statErr := opened.Stat()
			_ = opened.Close()
			if statErr == nil && prior.stamp.Size == info.Size() && prior.stamp.Mtime == info.ModTime().UnixNano() {
				return &agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: thread}, nil
			}
		}
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
	var done func()
	var operationErr error
	ctx, done, operationErr = l.beginOperation(ctx)
	if operationErr != nil {
		return nil, operationErr
	}
	defer done()
	if err := l.checkBudget(ctx); err != nil {
		return nil, err
	}
	var refs []agentapi.SourceRef
	for path := range l.byPhysical[id] {
		observation := l.observations[path]
		refs = append(refs, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: observation.identity.ThreadID})
	}
	if len(refs) > archive.MaxHistorySpans {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
	}
	slices.SortFunc(refs, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	return refs, nil
}

// Thread supplies current facts, plus bounded known same-thread candidates.
func (l *CodexRolloutLookup) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	var done func()
	var operationErr error
	ctx, done, operationErr = l.beginOperation(ctx)
	if operationErr != nil {
		return agentapi.CodexRolloutSet{}, operationErr
	}
	defer done()
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

	if l.coverage == nil {
		l.coverage = newCoverage(l.roots)
	}
	if l.coverage.request(id) {
		l.coverageDirty = true
	}
	l.registrationHint(ctx, id)
	var current *agentapi.SourceRef
	for _, root := range l.roots {
		view, err := l.index(ctx, root, false)
		if err != nil {
			if agentapi.Failure(err) == agentapi.Limit {
				return agentapi.CodexRolloutSet{}, err
			}
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
	if len(l.threads) >= maxCoverageRequests {
		for key := range l.threads {
			delete(l.threads, key)
			break
		}
	}
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
	for path := range l.byThread[id] {
		set.Candidates = append(set.Candidates, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id})
	}
	slices.SortFunc(set.Candidates, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	set.Complete = false
	if l.coverage != nil {
		if request, present := l.coverage.Requests[id]; present && request.CompleteEpoch == l.coverage.Epoch && !request.Overflow && l.coverage.Phase == "complete" && !l.coverage.Failed {
			set.Candidates = nil
			for _, candidate := range request.Candidates {
				set.Candidates = append(set.Candidates, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: candidate.Source.Locator, Key: id})
			}
			slices.SortFunc(set.Candidates, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
			set.Complete = true
		}
	}
	data, _ := json.Marshal(struct {
		Current      *agentapi.SourceRef
		Complete     bool
		Candidates   []agentapi.SourceRef
		Observations []rolloutObservationDigest
	}{Current: set.Current, Complete: set.Complete, Candidates: set.Candidates, Observations: l.candidateDigests(id)})
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
	if l.coverage != nil && l.coverage.Phase == "complete" && !l.coverage.Failed {
		if request, present := l.coverage.Requests[id]; present && request.CompleteEpoch == l.coverage.Epoch && !request.Overflow {
			for path, candidate := range request.Candidates {
				out = append(out, rolloutObservationDigest{path, candidate.Stamp, candidate.Identity})
			}
			slices.SortFunc(out, func(a, b rolloutObservationDigest) int { return strings.Compare(a.Path, b.Path) })
			return out
		}
	}
	for path := range l.byThread[id] {
		o := l.observations[path]
		out = append(out, rolloutObservationDigest{path, o.stamp, o.identity})
	}
	slices.SortFunc(out, func(a, b rolloutObservationDigest) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Check revalidates selection within the pass. One private refresh per home is
// allowed; subsequent changes remain pending instead of making unbounded copies.
func (l *CodexRolloutLookup) Check(ctx context.Context, id, revision string) error {
	var done func()
	var operationErr error
	ctx, done, operationErr = l.beginOperation(ctx)
	if operationErr != nil {
		return operationErr
	}
	defer done()
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
	snapshot, err := snapshotCurrentIndexBudget(ctx, root, nil, l.readBudget)
	if err != nil {
		if agentapi.Failure(err) == agentapi.Limit {
			delete(l.indexes, root)
			return nil, err
		}
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
	if l.coverageDirty {
		if err := l.coverage.validate(l.roots); err != nil {
			return errors.Join(append(errs, err)...)
		}
		var current catalog
		err := local.Read(filepath.Join(l.store.Home(), "discovery-catalog.json"), &current)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			if current.Coverage != nil && current.Coverage.Version != 1 {
				errs = append(errs, errors.New("native coverage requires a newer writer"))
			} else {
				current.Version = catalogVersion
				current.Roots = slices.Clone(l.roots)
				current.Coverage = l.coverage
				errs = append(errs, local.Write(filepath.Join(l.store.Home(), "discovery-catalog.json"), current))
			}
		} else {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var _ agentapi.CodexRolloutLookup = (*CodexRolloutLookup)(nil)

func (l *CodexRolloutLookup) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if err := l.checkBudget(ctx); err != nil {
		return ctx, nil, err
	}
	started := time.Now()
	deadline := started.Add(l.remaining)
	l.deadline = deadline
	operation, cancel := context.WithDeadline(ctx, deadline)
	return operation, func() { cancel(); l.remaining -= time.Since(started) }, nil
}

// NativeReadBudget is the pass-owned shared native/source/cache charge ledger.
func (l *CodexRolloutLookup) NativeReadBudget() *agentapi.NativeReadBudget { return l.readBudget }
