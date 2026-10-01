// Package discovery performs bounded local source observation and admission.
// Adapters supply source facts; current configuration authorizes capture.
package discovery

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

const (
	// Budget reserves collector time for queued publications and retention.
	Budget = 5 * time.Second
	// HeaderProbes caps metadata reads in one pass.
	HeaderProbes   = 256
	maxCatalog     = 8192
	maxDirectories = 4096
)

// Health separates scan coverage from upload and hook health. Codes never
// include native IDs, paths or native operating-system errors.
type Health struct {
	Enabled        bool           `json:"enabled"`
	Supported      bool           `json:"supported"`
	LastAttempt    time.Time      `json:"last_attempt,omitzero"`
	LastReconciled time.Time      `json:"last_reconciled,omitzero"`
	Pending        bool           `json:"pending"`
	Probes         int            `json:"header_probes"`
	Entries        int            `json:"directory_entries"`
	Bytes          int64          `json:"bytes_read"`
	Registered     int            `json:"registered"`
	Outcomes       map[string]int `json:"outcomes,omitempty"`
	Errors         []string       `json:"errors,omitempty"`
}

type directory struct {
	Root   string `json:"root"`
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}
type cached struct {
	Size       int64              `json:"size"`
	Mtime      int64              `json:"mtime"`
	Checked    time.Time          `json:"checked"`
	Header     sourcefacts.Header `json:"header"`
	ActiveHint bool               `json:"active_hint,omitempty"`
}
type catalog struct {
	Roots    []string          `json:"roots"`
	Queue    []directory       `json:"queue"`
	Cache    map[string]cached `json:"cache"`
	Health   Health            `json:"health"`
	Priority map[string]int64  `json:"priority,omitempty"`
}

// Options contains injectable clocks and stop signals; it never enables an
// unverified producer. The supported registry is sourcefacts' release gate.
type Options struct {
	Now  func() time.Time
	Stop func() bool
}

// Run observes configured Codex homes under the caller's collector lock,
// before any storage initialization. Each admission revalidates permissions
// under hooks.lock, never holding it while reading source files.
func Run(ctx context.Context, store *state.Store, cfg config.Config, o Options) (Health, error) {
	return run(ctx, store, cfg, o, sourcefacts.SupportedCodexProducer)
}

// run's contract seam is private: only synthetic tests may inject support.
func run(ctx context.Context, store *state.Store, cfg config.Config, o Options, supported func(sourcefacts.CodexMeta) bool) (Health, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if cfg.Discovery == nil || !cfg.Discovery.Enabled {
		return Health{}, nil
	}
	now := o.Now().UTC()
	h := Health{Enabled: true, LastAttempt: now, Outcomes: map[string]int{}}
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	var c catalog
	if err := local.Read(path, &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.Errors = append(h.Errors, "catalog_rebuilt")
	}
	roots := approvedRoots(cfg.Discovery.CodexHomes)
	if len(c.Cache) > maxCatalog || len(c.Queue) > maxDirectories || len(c.Priority) > 64 || !slices.Equal(c.Roots, roots) {
		c = catalog{}
	}
	c.Roots = roots
	if c.Cache == nil {
		c.Cache = map[string]cached{}
	}
	h.LastReconciled = c.Health.LastReconciled
	if len(c.Queue) == 0 {
		for _, root := range roots {
			c.Queue = append(c.Queue, directory{Root: root, Path: "sessions"}, directory{Root: root, Path: "archived_sessions"})
		}
	}
	deadline := time.Now().Add(Budget)
	priority := scan{store: store, cfg: cfg, catalog: &c, health: &h, now: now, supported: supported, priority: true, ctx: ctx}
	priority.observeActiveHints(ctx, o, roots, deadline)

	for len(c.Queue) > 0 && h.Probes < HeaderProbes && h.Entries < 2048 && time.Now().Before(deadline) {
		if scanStopped(ctx, o) {
			break
		}
		d := c.Queue[0]
		c.Queue = c.Queue[1:]
		names, next, finished, err := readBatch(d)
		if err != nil {
			h.Errors = appendUnique(h.Errors, "source_root_unavailable")
			continue
		}
		worker := scan{store: store, cfg: cfg, catalog: &c, health: &h, now: now, supported: supported, ctx: ctx}
		for _, name := range names {
			if scanStopped(ctx, o) || time.Now().After(deadline) {
				finished = false
				next = d.Offset
				break
			}
			retry, stop := worker.visit(d, name)
			if retry {
				finished = false
				next = d.Offset
			}
			if stop {
				break
			}
		}

		if !finished {
			d.Offset = next
			c.Queue = append(c.Queue, d)
		}
	}
	h.Pending = len(c.Queue) > 0
	if !h.Pending && len(h.Errors) == 0 {
		h.LastReconciled = now
	}
	pruneCatalog(&c)
	c.Health = h
	if err := local.Write(path, c); err != nil {
		return h, errors.New("discovery state write failed; retry next scan")
	}
	return h, nil
}

func scanStopped(ctx context.Context, o Options) bool {
	return ctx.Err() != nil || (o.Stop != nil && o.Stop())
}

func approvedRoots(homes []string) []string {
	var roots []string
	for _, home := range homes {
		if !filepath.IsAbs(home) {
			continue
		}
		root, err := filepath.EvalSymlinks(home)
		if err != nil {
			root = filepath.Clean(home)
		}
		roots = appendUnique(roots, root)
	}
	return roots
}
func appendUnique(values []string, value string) []string {
	if !slices.Contains(values, value) {
		values = append(values, value)
	}
	return values
}

// Directory cookies continue bounded enumeration without rereading preceding
// names. They are hints: every completed round restarts reconciliation, so
// moves, directory replacement and invalidation cannot silently lose coverage.
func readBatch(d directory) ([]string, int64, bool, error) {
	root, err := os.OpenRoot(d.Root)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(d.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, true, nil
	}
	if err != nil || !info.IsDir() {
		return nil, 0, false, errors.New("not a directory")
	}
	f, err := root.Open(d.Path)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(d.Offset, io.SeekStart); err != nil {
		return nil, 0, false, err
	}
	buffer := make([]byte, 4096)
	n, err := syscall.ReadDirent(int(f.Fd()), buffer)
	if err != nil {
		return nil, 0, false, err
	}
	_, _, names := syscall.ParseDirent(buffer[:n], -1, nil)
	offset, err := f.Seek(0, io.SeekCurrent)
	return names, offset, n == 0, err
}

func resolveProject(cfg config.Config, cwd string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", false
	}
	resolve := func(p string) string {
		r, e := filepath.EvalSymlinks(p)
		if e != nil {
			return filepath.Clean(p)
		}
		return r
	}
	if p, ok := sourcefacts.ConfiguredOwner(cfg.Archive.Projects, resolved, resolve); ok {
		return p.Root, p.Included
	}
	for d, depth := resolved, 0; depth < 64; depth++ {
		path := filepath.Join(d, ".git")
		info, e := os.Lstat(path)
		if e == nil {
			if info.IsDir() {
				if p, ok := sourcefacts.ConfiguredOwner(cfg.Archive.Projects, d, resolve); ok {
					return p.Root, p.Included
				}
				return "", false
			}
			read := func(path string) ([]byte, error) {
				f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if e != nil {
					return nil, e
				}
				defer func() { _ = f.Close() }()
				info, e := f.Stat()
				if e != nil || !info.Mode().IsRegular() {
					return nil, errors.New("invalid Git metadata")
				}
				return io.ReadAll(io.LimitReader(f, 4097))
			}
			exists := func(path string) bool { info, e := os.Stat(path); return e == nil && info.IsDir() }
			main, ok := sourcefacts.WorktreeMain(d, read, exists, true)
			if !ok {
				return "", false
			}
			if p, ok := sourcefacts.ConfiguredOwner(cfg.Archive.Projects, resolve(main), resolve); ok {
				return p.Root, p.Included
			}
			return "", false
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", false
}

func admit(store *state.Store, header sourcefacts.Header, sourceRoot, locator, project, generation string, now time.Time) (archive.SessionRegistration, bool, error) {
	previous, replace := continuationLocator(store, header.Meta.ID, sourceRoot, locator)
	unlock, err := local.NamedLock(store.Home(), "hooks.lock")
	if err != nil {
		return archive.SessionRegistration{}, false, err
	}
	defer unlock()
	if setupjournal.TransactionPending(store.Home()) {
		return archive.SessionRegistration{}, false, errors.New("setup pending")
	}
	cfg, found, err := config.Load(store.Home())
	if err != nil || !found {
		return archive.SessionRegistration{}, false, errors.New("configuration unavailable")
	}
	id, exists, err := store.AgentSessionID("codex", header.Meta.ID)
	if err != nil {
		return archive.SessionRegistration{}, false, err
	}
	if exists {
		if reg, found, e := store.LoadRegistration(id); e != nil {
			return reg, false, e
		} else if found {
			if reg.ProjectRoot != project || !cfg.AcceptSession(reg) {
				return reg, false, errors.New("identity conflict")
			}
			if reg.Origin == archive.SessionOriginDiscovery && (reg.DiscoveryCwd != header.Meta.Cwd || !reg.SessionStartedAt.Equal(header.Started)) {
				return reg, false, errors.New("continuation identity conflict")
			}
			if reg.TranscriptPath == "" || (replace && reg.TranscriptPath == previous && reg.Origin == archive.SessionOriginDiscovery) {
				_, e = store.UpdateRegistration(id, func(r *archive.SessionRegistration) error {
					if r.TranscriptPath == reg.TranscriptPath {
						r.TranscriptPath = locator
						if r.Origin == archive.SessionOriginDiscovery {
							r.DiscoveryRoot = sourceRoot
						}
					}
					reg = *r
					return nil
				})
				return reg, false, e
			}
			return reg, false, nil
		}
	}
	current, allowed := cfg.DiscoveryGeneration("codex", project, header.Started, now)
	if !allowed || current != generation {
		return archive.SessionRegistration{}, false, errors.New("authorization changed")
	}
	reg, err := store.RegisterOrMerge(header.Meta.ID, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id, NativeSessionID: header.Meta.ID, Harness: archive.Harness{Name: "codex", Version: header.Meta.Version}, ProjectID: archive.ProjectID(project), ProjectRoot: project,
			TranscriptPath: locator, DiscoveryRoot: sourceRoot, DiscoveryCwd: header.Meta.Cwd, DiscoveryGeneration: generation, SessionStartedAt: header.Started, RegisteredAt: now, AdmittedAt: now,
			Origin: archive.SessionOriginDiscovery, StartedAtSource: archive.StartedAtSourceTranscript, DestinationID: cfg.DestinationID(),
		}
	})
	// A registration without a request remains discoverable by collector's full
	// registration pass, so a crash here cannot lose the first publication.
	if err == nil {
		err = store.SaveRequest(reg.ArchiveSessionID, "discovery", now)
	}
	return reg, true, err
}

// Source existence and active/archive preference are continuation hints only.
// Read them before hooks.lock; publication revalidates the chosen snapshot.
func continuationLocator(store *state.Store, native, sourceRoot, locator string) (string, bool) {
	id, found, err := store.AgentSessionID("codex", native)
	if err != nil || !found {
		return "", false
	}
	r, found, err := store.LoadRegistration(id)
	if err != nil || !found || r.Origin != archive.SessionOriginDiscovery || r.TranscriptPath == locator {
		return "", false
	}
	_, err = os.Lstat(r.TranscriptPath)
	if errors.Is(err, os.ErrNotExist) {
		return r.TranscriptPath, true
	}
	return r.TranscriptPath, local.PathWithin(locator, filepath.Join(sourceRoot, "sessions")) && local.PathWithin(r.TranscriptPath, filepath.Join(r.DiscoveryRoot, "archived_sessions"))
}

// LoadHealth reads scan facts independently of collector/upload status.
func LoadHealth(home string) Health {
	var c catalog
	if local.Read(filepath.Join(home, "discovery-catalog.json"), &c) != nil {
		return Health{}
	}
	return c.Health
}

type scan struct {
	store     *state.Store
	cfg       config.Config
	catalog   *catalog
	health    *Health
	now       time.Time
	supported func(sourcefacts.CodexMeta) bool
	ctx       context.Context
	priority  bool
}

func (s scan) visit(d directory, name string) (retry, stop bool) {
	store, cfg, c, h, now, supported := s.store, s.cfg, s.catalog, s.health, s.now, s.supported
	h.Entries++
	loc := filepath.Join(d.Root, d.Path, name)
	info, e := os.Lstat(loc)
	if e != nil {
		return false, false
	}
	if info.IsDir() {
		if s.priority {
			return false, false
		}
		if len(c.Queue) >= maxDirectories || strings.Count(d.Path, string(filepath.Separator)) >= 16 {
			h.Errors = appendUnique(h.Errors, "directory_limit")
			return false, false
		}
		c.Queue = append(c.Queue, directory{Root: d.Root, Path: filepath.Join(d.Path, name)})
		return false, false
	}
	if !info.Mode().IsRegular() || !strings.HasPrefix(name, "rollout-") || sourcefacts.RolloutID(name) == "" {
		return false, false
	}
	entry, hit := c.Cache[loc]
	retryDelay := time.Hour
	if entry.Header.Outcome == "incomplete_metadata" || entry.Header.Outcome == "source_unavailable" || entry.Header.Outcome == "source_changed" {
		retryDelay = time.Minute
	}
	if !hit || entry.Size != info.Size() || entry.Mtime != info.ModTime().UnixNano() || now.Sub(entry.Checked) >= retryDelay || now.Before(entry.Checked) {
		if h.Probes >= HeaderProbes {
			return true, true
		}
		header := sourcefacts.ReadHeader(s.ctx, d.Root, loc)
		h.Probes++
		h.Bytes += header.Bytes
		entry = cached{Size: info.Size(), Mtime: info.ModTime().UnixNano(), Checked: now, Header: header}
		c.Cache[loc] = entry
	}
	if s.priority && !entry.ActiveHint {
		entry.ActiveHint = true
		c.Cache[loc] = entry
	}
	h.Outcomes[entry.Header.Outcome]++
	if entry.Header.Outcome != "native_format" {
		return false, false
	}
	if !supported(entry.Header.Meta) {
		h.Outcomes["unsupported_producer"]++
		return false, false
	}
	h.Supported = true
	root, ok := resolveProject(cfg, entry.Header.Meta.Cwd)
	if !ok {
		h.Outcomes["project_not_authorized"]++
		return false, false
	}
	generation, authorized := cfg.DiscoveryGeneration("codex", root, entry.Header.Started, now)
	if !authorized {
		_, existing, err := store.AgentSessionID("codex", entry.Header.Meta.ID)
		if err != nil || !existing {
			h.Outcomes["start_not_authorized"]++
			return false, false
		}
	}
	reg, created, e := admit(store, entry.Header, d.Root, loc, root, generation, now)
	_ = reg
	if e != nil {
		h.Outcomes["admission_retry"]++
		delete(c.Cache, loc)
		return true, false
	} else if created {
		h.Registered++
	}
	return false, false
}

func pruneCatalog(c *catalog) {
	if len(c.Cache) > maxCatalog {
		keys := make([]string, 0, len(c.Cache))
		for key := range c.Cache {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if c.Cache[a].ActiveHint != c.Cache[b].ActiveHint {
				if c.Cache[a].ActiveHint {
					return 1
				}
				return -1
			}
			return c.Cache[a].Checked.Compare(c.Cache[b].Checked)
		})
		for _, key := range keys[:len(keys)-maxCatalog] {
			delete(c.Cache, key)
		}
	}
}
