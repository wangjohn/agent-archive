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

	"github.com/wangjohn/agent-archive/internal/agentmeta"
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
	HeaderProbes = 256
	// Reprobe older cached observations so they acquire explicit format profiles.
	catalogVersion = 6
	maxCatalog     = 8192
	maxDirectories = 4096
	maxRetries     = 256
)

// Health separates scan coverage from upload and hook health. Codes never
// include native IDs, paths or native operating-system errors.
type Health struct {
	Enabled           bool                `json:"enabled"`
	Supported         bool                `json:"supported"`
	LastAttempt       time.Time           `json:"last_attempt,omitzero"`
	LastReconciled    time.Time           `json:"last_reconciled,omitzero"`
	Pending           bool                `json:"pending"`
	ProjectOperations int                 `json:"project_metadata_operations"`
	GitBytes          int                 `json:"git_metadata_bytes"`
	Probes            int                 `json:"header_probes"`
	Entries           int                 `json:"directory_entries"`
	IndexBytes        int64               `json:"index_bytes_read,omitempty"`
	IndexQueries      int                 `json:"index_queries,omitempty"`
	IndexLocators     int                 `json:"index_locators,omitempty"`
	Bytes             int64               `json:"bytes_read"`
	Registered        int                 `json:"registered"`
	Outcomes          map[string]int      `json:"outcomes,omitempty"`
	Errors            []string            `json:"errors,omitempty"`
	Formats           []FormatObservation `json:"observed_formats,omitempty"`
}

type directory struct {
	Root   string `json:"root"`
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

type cached struct {
	Size        int64       `json:"size"`
	Mtime       int64       `json:"mtime"`
	Checked     time.Time   `json:"checked"`
	Observation Observation `json:"observation"`
	ActiveHint  bool        `json:"active_hint,omitempty"`
}

type catalog struct {
	Coverage *coverageInventory `json:"native_coverage,omitempty"`
	Version  int                `json:"version"`
	Roots    []string           `json:"roots"`
	Queue    []directory        `json:"queue"`
	Cache    map[string]cached  `json:"cache"`
	Health   Health             `json:"health"`
	Priority map[string]int64   `json:"priority,omitempty"`
	Retries  []SourceDescriptor `json:"retries,omitempty"`
}

// Options contains injectable clocks and stop signals; it cannot override
// source format support or authorize capture.
type Options struct {
	// Rollouts receives the same validated observations and bounded coverage state.
	Rollouts *CodexRolloutLookup
	Now      func() time.Time
	Stop     func() bool
}

// Run observes configured Codex homes under the caller's collector lock,
// before any storage initialization. Each admission revalidates permissions
// under hooks.lock, never holding it while reading source files.
func Run(ctx context.Context, store *state.Store, cfg config.Config, o Options) (Health, error) {
	return runWithAdapters(ctx, store, cfg, o, registeredAdapters())
}

func runWithAdapters(ctx context.Context, store *state.Store, cfg config.Config, o Options, adapters []SourceAdapter) (Health, error) {
	adapter := findAdapter(adapters, "codex")
	if adapter == nil {
		return Health{}, nil
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if cfg.Discovery == nil || !cfg.Discovery.Enabled {
		return Health{}, nil
	}
	now := o.Now().UTC()
	h := Health{Enabled: true, LastAttempt: now, Outcomes: map[string]int{}}
	c, path, roots, err := prepareCatalog(store, cfg, o, adapter, &h)
	if err != nil {
		return h, err
	}
	allowance := Budget
	if o.Rollouts != nil {
		allowance = min(Budget-time.Second, o.Rollouts.remaining-time.Second)
		if allowance <= 0 {
			return h, errors.New("native observation budget exhausted")
		}
	}
	started := time.Now()
	if o.Rollouts != nil {
		defer func() { o.Rollouts.remaining -= time.Since(started) }()
	}
	deadline := started.Add(allowance)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	resolver := sourcefacts.NewProjectResolver()
	priority := scan{resolver: resolver, store: store, cfg: cfg, catalog: &c, health: &h, now: now, adapter: adapter, priority: true, ctx: ctx, rollouts: o.Rollouts}
	priority.observeIndexHints(ctx, o, roots, deadline)
	priority.observeActiveHints(ctx, o, roots, deadline)
	priority.observeRetries(o)

	priority.observeDirectories(o, deadline)
	h.ProjectOperations = resolver.Operations
	h.GitBytes = resolver.GitBytes
	h.Pending = len(c.Queue) > 0 || len(c.Retries) > 0 || c.Coverage != nil && c.Coverage.Phase != "complete"
	if !h.Pending && len(h.Errors) == 0 {
		h.LastReconciled = now
	}
	pruneCatalog(&c)
	c.Health = h
	if c.Coverage != nil {
		if err := c.Coverage.validate(roots); err != nil {
			return h, err
		}
	}
	if err := local.Write(path, c); err != nil {
		return h, errors.New("discovery state write failed; retry next scan")
	}
	if err := writeHealth(store.Home(), h); err != nil {
		return h, err
	}
	return h, nil
}

func catalogNeedsReset(c catalog, roots []string) bool {
	return c.Version != catalogVersion || len(c.Cache) > maxCatalog || len(c.Queue) > maxDirectories || len(c.Priority) > 64 || len(c.Retries) > maxRetries || !slices.Equal(c.Roots, roots)
}

func validSource(source SourceDescriptor, root string) bool {
	return source.Root == root && filepath.IsAbs(source.Locator) && local.PathWithin(source.Locator, root) && source.Kind == archive.SourceKindFile && source.StableKey != "" && len(source.StableKey) <= 4096
}

func validCandidate(candidate Candidate, source SourceDescriptor, agent string, now time.Time) bool {
	return candidate.Agent == agent && candidate.Source == source && candidate.NativeSessionID != "" && len(candidate.NativeSessionID) <= 4096 && filepath.IsAbs(candidate.WorkingDirectory) && len(candidate.WorkingDirectory) <= 4096 && !candidate.StartedAt.IsZero() && !candidate.FirstTaskAt.IsZero() && !candidate.FirstTaskAt.Before(candidate.StartedAt.Add(-time.Second)) && !candidate.FirstTaskAt.After(now.Add(2*time.Minute)) && candidate.StartEvidence == "native_start" && candidate.Execution == "native" && candidate.ParentNativeID == "" && candidate.ForkNativeID == ""
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
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		facts, ok := sourcefacts.PhysicalProject(cwd)
		if !ok || !cfg.CodexProjectAllowed(facts.Root, facts.Cwd, time.Now().Add(100*365*24*time.Hour)) {
			return "", false
		}
		return facts.Root, true
	}
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

func sessionKey(agent, native string) agentmeta.SessionKey {
	return agentmeta.SessionKey{Agent: agentmeta.ID(agent), NativeID: native}
}

func admitSession(store *state.Store, candidate Candidate, project, generation string, now time.Time, factSnapshots ...sourcefacts.ProjectFacts) (bool, error) {
	var cfgFacts *sourcefacts.ProjectFacts
	if len(factSnapshots) > 0 {
		cfgFacts = &factSnapshots[0]
	}
	var facts sourcefacts.ProjectFacts
	var factsOK bool
	if cfgFacts != nil {
		facts = *cfgFacts
		factsOK = true
	} else {
		facts, factsOK = sourcefacts.PhysicalProject(candidate.WorkingDirectory)
	}
	sourceRoot, locator := candidate.Source.Root, candidate.Source.Locator
	previous, replace := continuationLocator(store, candidate.Agent, candidate.NativeSessionID, candidate.Source)
	unlock, err := local.NamedLock(store.Home(), "hooks.lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	if setupjournal.TransactionPending(store.Home()) {
		return false, errors.New("setup pending")
	}
	cfg, found, err := config.Load(store.Home())
	if err != nil || !found {
		return false, errors.New("configuration unavailable")
	}
	if cfg.Paused || !cfg.Archive.Enabled {
		return false, errors.New("capture authorization is inactive")
	}
	if _, removed, err := store.Removal(candidate.Agent, candidate.NativeSessionID); err != nil {
		return false, err
	} else if removed {
		return false, errors.New("session was removed")
	}
	id, exists, err := store.ArchiveSessionID(sessionKey(candidate.Agent, candidate.NativeSessionID))
	if errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		err = errors.Join(err, store.RequestSessionIndexRecovery(sessionKey(candidate.Agent, candidate.NativeSessionID)))
	}
	if err != nil {
		return false, err
	}
	if exists {
		_, found, err := mergeContinuation(store, cfg, candidate, project, continuationHint{ArchiveID: id, Previous: previous, Replace: replace, CanonicalCwd: facts.Cwd}, now)
		if found || err != nil {
			return false, err
		}
	}
	// A silent loss of both derived indexes is indistinguishable from a new
	// identity. Only the existing registration census can authorize that miss.
	absent, err := store.SessionIndexAbsent(sessionKey(candidate.Agent, candidate.NativeSessionID))
	if err != nil {
		return false, err
	}
	if !absent {
		err := store.RequestSessionIndexRecovery(sessionKey(candidate.Agent, candidate.NativeSessionID))
		return false, errors.Join(state.ErrSessionIndexRecoveryRequired, err)
	}
	current, allowed := cfg.DiscoveryGeneration(candidate.Agent, project, candidate.StartedAt, now)
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		if !factsOK || facts.Root != project || cfg.Discovery == nil || !cfg.Discovery.Enabled {
			return false, errors.New("project facts changed")
		}
		current, allowed = cfg.CodexDiscoveryGeneration(facts.Root, facts.Cwd, candidate.StartedAt, now)
	}
	if !allowed || current != generation {
		return false, errors.New("authorization changed")
	}
	reg, err := store.RegisterOrMerge(sessionKey(candidate.Agent, candidate.NativeSessionID), func(id string) archive.SessionRegistration {
		var proof *archive.CodexAdmissionProof
		if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			proof = &archive.CodexAdmissionProof{Generation: generation, Revision: cfg.CodexCapture.Revision, Cwd: facts.Cwd}
		}
		return archive.SessionRegistration{CodexAdmission: proof,
			ArchiveSessionID: id, NativeSessionID: candidate.NativeSessionID, Harness: archive.Harness{Name: candidate.Agent, Version: candidate.HarnessVersion}, ProjectID: archive.ProjectID(project), ProjectRoot: project,
			SourceKind: candidate.Source.Kind, SourceKey: candidate.Source.StableKey, TranscriptPath: locator, DiscoveryRoot: sourceRoot, DiscoveryCwd: candidate.WorkingDirectory, DiscoveryProducerOriginator: candidate.ProducerOriginator, DiscoveryProducerSource: candidate.ProducerSource, DiscoveryGeneration: generation, DiscoverySourcePriority: candidate.Source.Priority, SessionStartedAt: candidate.StartedAt, RegisteredAt: now, AdmittedAt: now,
			Origin: archive.SessionOriginDiscovery, StartedAtSource: archive.StartedAtSourceTranscript, DestinationID: cfg.DestinationID(),
		}
	})
	// A registration without a request remains discoverable by collector's full
	// registration pass, so a crash here cannot lose the first publication.
	if err == nil {
		err = store.SaveRequest(reg.ArchiveSessionID, "discovery", now)
	}
	return true, err
}

type continuationHint struct {
	CanonicalCwd string
	ArchiveID    string
	Previous     string
	Replace      bool
}

// mergeContinuation runs under hooks.lock. Only the source locator can change;
// original origin, admission, start and destination attribution stay immutable.
func mergeContinuation(store *state.Store, cfg config.Config, candidate Candidate, project string, hint continuationHint, now time.Time) (archive.SessionRegistration, bool, error) {
	reg, found, err := store.LoadRegistration(hint.ArchiveID)
	if err != nil || !found {
		return reg, found, err
	}
	compatible := reg.ProjectRoot == project || (reg.CodexAdmission == nil && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects && local.PathWithin(candidate.WorkingDirectory, reg.ProjectRoot))
	permission := true
	if reg.CodexAdmission != nil {
		permission = hint.CanonicalCwd != "" && cfg.CodexContinuationAllowed(project, hint.CanonicalCwd)
	} else if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		permission = cfg.CodexContinuationAllowed(project, hint.CanonicalCwd)
	}
	if !compatible || !cfg.AcceptSession(reg) || !permission {
		return reg, true, errors.New("identity conflict")
	}
	// A discovery observation cannot establish an unrestricted hook/import
	// locator. Those origins retain their own source authority and attribution.
	if reg.Origin != archive.SessionOriginDiscovery {
		return reg, true, nil
	}
	if reg.DiscoveryCwd != candidate.WorkingDirectory || !reg.SessionStartedAt.Equal(candidate.StartedAt) {
		return reg, true, errors.New("continuation identity conflict")
	}
	if reg.TranscriptPath == "" || (hint.Replace && reg.TranscriptPath == hint.Previous && reg.Origin == archive.SessionOriginDiscovery) {
		// Invalidate before moving the locator: a crash must never leave the
		// replacement trusted through the old file's same size/mtime signature.
		if err := store.RemoveScanSignature(hint.ArchiveID); err != nil {
			return reg, true, err
		}
		if err := store.SaveRequest(hint.ArchiveID, "discovery", now); err != nil {
			return reg, true, err
		}
		_, err = store.UpdateRegistration(hint.ArchiveID, func(r *archive.SessionRegistration) error {
			if r.TranscriptPath == reg.TranscriptPath {
				r.TranscriptPath = candidate.Source.Locator
				if r.Origin == archive.SessionOriginDiscovery {
					r.DiscoveryRoot = candidate.Source.Root
					r.DiscoverySourcePriority = candidate.Source.Priority
				}
			}
			reg = *r
			return nil
		})
	}
	return reg, true, err
}

// Source existence and active/archive preference are continuation hints only.
// Read them before hooks.lock; publication revalidates the chosen snapshot.
func continuationLocator(store *state.Store, agent, native string, source SourceDescriptor) (string, bool) {
	locator := source.Locator
	id, found, err := store.ArchiveSessionID(sessionKey(agent, native))
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
	return r.TranscriptPath, source.Priority < r.DiscoverySourcePriority
}

type scan struct {
	rollouts            *CodexRolloutLookup
	resolver            *sourcefacts.ProjectResolver
	store               *state.Store
	cfg                 config.Config
	catalog             *catalog
	health              *Health
	now                 time.Time
	adapter             SourceAdapter
	ctx                 context.Context
	priority            bool
	reservedDirectories int
}

func (s scan) visitEntry(d directory, source SourceEntry) (retry, stop bool) {
	c, h, now := s.catalog, s.health, s.now
	h.Entries++
	if source.Directory != "" {
		s.enqueueDirectory(d, source.Directory)
		return false, false
	}
	if source.Source.Locator == "" {
		return false, false
	}
	loc := source.Source.Locator
	if !validSource(source.Source, d.Root) {
		h.Outcomes["invalid_source"]++
		return false, false
	}
	entry, hit := c.Cache[loc]
	retryDelay := time.Hour
	if entry.Observation.Outcome == outcomeIncomplete || entry.Observation.Outcome == outcomeUnavailable || entry.Observation.Outcome == outcomeChanged {
		retryDelay = time.Minute
	}
	if !hit || entry.Size != source.Fingerprint.Size || entry.Mtime != source.Fingerprint.Mtime || now.Sub(entry.Checked) >= retryDelay || now.Before(entry.Checked) {
		if !discoveryProbeAvailable(h, s.rollouts) {
			return true, true
		}
		observation := s.adapter.Inspect(s.ctx, source.Source)
		h.Probes++
		if s.rollouts != nil {
			s.rollouts.probes++
		}
		h.Bytes += observation.Bytes
		entry = cached{Size: source.Fingerprint.Size, Mtime: source.Fingerprint.Mtime, Checked: now, Observation: observation}
		c.Cache[loc] = entry
	}
	if s.priority && !entry.ActiveHint {
		entry.ActiveHint = true
		c.Cache[loc] = entry
	}
	if s.rollouts != nil {
		s.rollouts.Observe(source.Source, source.Fingerprint, entry.Observation.Identity)
	}
	h.Outcomes[string(entry.Observation.Outcome)]++
	if entry.Observation.Outcome != outcomeUsable {
		return false, false
	}
	candidate := entry.Observation.Candidate
	// Shared structural checks do not trust an adapter to select another source
	// or authorize inherited/unknown evidence. Project/destination policy stays here.
	if !validCandidate(candidate, source.Source, s.adapter.Agent(), now) {
		h.Outcomes["invalid_candidate"]++
		return false, false
	}
	if !s.adapter.Supported(candidate) {
		h.Outcomes["unsupported_producer"]++
		return false, false
	}
	h.Supported = true
	h.observeFormat(candidate)
	if s.removalBlocks(candidate) {
		return false, false
	}
	return s.admitCandidate(candidate, loc)
}

// Retained admission retries rotate separately from directory enumeration.
// One conflicting or contended source must not pin a directory's coverage.
func (s scan) retainRetry(source SourceDescriptor) {
	for _, prior := range s.catalog.Retries {
		if prior.Locator == source.Locator {
			return
		}
	}
	if len(s.catalog.Retries) == maxRetries {
		s.health.Errors = appendUnique(s.health.Errors, "retry_limit")
		return // Full filesystem reconciliation will revisit overflow sources.
	}
	s.catalog.Retries = append(s.catalog.Retries, source)
}

func (s scan) observeRetries(o Options) {
	worker := s
	worker.priority = false
	// Process only the initial queue once and reserve half the probes for
	// forward coverage, even when all retained retries remain conflicting.
	for remaining := min(len(s.catalog.Retries), 64); remaining > 0 && s.health.Probes < HeaderProbes/2 && !scanStopped(s.ctx, o); remaining-- {
		source := s.catalog.Retries[0]
		s.catalog.Retries = s.catalog.Retries[1:]
		if !slices.Contains(s.catalog.Roots, source.Root) || !validSource(source, source.Root) {
			continue
		}
		path, err := filepath.Rel(source.Root, filepath.Dir(source.Locator))
		if err != nil {
			continue
		}
		entry := s.adapter.Describe(source.Root, path, filepath.Base(source.Locator))
		worker.visitEntry(directory{Root: source.Root, Path: path}, entry)
	}
}

func (s scan) enqueueDirectory(d directory, path string) {
	if s.priority {
		return
	}
	if filepath.IsAbs(path) || filepath.Clean(path) != path || !local.PathWithin(filepath.Join(d.Root, path), d.Root) {
		s.health.Outcomes["invalid_source"]++
		return
	}
	if len(s.catalog.Queue)+s.reservedDirectories >= maxDirectories || strings.Count(d.Path, string(filepath.Separator)) >= 16 {
		s.health.Errors = appendUnique(s.health.Errors, "directory_limit")
		return
	}
	s.catalog.Queue = append(s.catalog.Queue, directory{Root: d.Root, Path: path})
}

// removalBlocks avoids repeat census work for a known removed source. Retention
// and removal share collector.lock with the scanner's production caller.
func (s scan) removalBlocks(candidate Candidate) bool {
	if _, removed, err := s.store.Removal(candidate.Agent, candidate.NativeSessionID); err != nil {
		s.health.Outcomes["removal_unavailable"]++
		s.health.Errors = appendUnique(s.health.Errors, "removal_unavailable")
		return true
	} else if removed {
		s.health.Outcomes["removed_session"]++
		return true
	}
	return false
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

func (s scan) admitCandidate(candidate Candidate, loc string) (bool, bool) {
	cfg, store, h, c, now := s.cfg, s.store, s.health, s.catalog, s.now
	var facts sourcefacts.ProjectFacts
	var root string
	var ok bool
	physical := cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects
	if id, known, e := store.ArchiveSessionID(sessionKey(candidate.Agent, candidate.NativeSessionID)); e == nil && known {
		if prior, found, e := store.LoadRegistration(id); e == nil && found {
			if !cfg.AcceptSession(prior) {
				h.Outcomes["project_not_authorized"]++
				return false, false
			}
			if prior.CodexAdmission != nil {
				physical = true
			}
		}
	}

	if physical {
		facts, ok = s.resolver.Resolve(candidate.WorkingDirectory)
		root = facts.Root
		if !ok && s.resolver.Exhausted {
			h.Outcomes["project_budget_exhausted"]++
			s.retainRetry(candidate.Source)
			return false, false
		}
	} else {
		root, ok = resolveProject(cfg, candidate.WorkingDirectory)
	}
	if !ok {
		if physical {
			h.Outcomes["project_facts_unavailable"]++
			return false, false
		}
		h.Outcomes["project_not_authorized"]++
		return false, false
	}
	generation, authorized := cfg.DiscoveryGeneration(candidate.Agent, root, candidate.StartedAt, now)
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		generation, authorized = cfg.CodexDiscoveryGeneration(root, facts.Cwd, candidate.StartedAt, now)
	}
	if !authorized {
		_, existing, err := store.ArchiveSessionID(sessionKey(candidate.Agent, candidate.NativeSessionID))
		if err != nil || !existing {
			h.Outcomes["start_not_authorized"]++
			return false, false
		}
	}
	var created bool
	var e error
	if physical {
		created, e = admitSession(store, candidate, root, generation, now, facts)
	} else {
		created, e = admitSession(store, candidate, root, generation, now)
	}
	if e != nil {
		h.Outcomes["admission_retry"]++
		delete(c.Cache, loc)
		s.retainRetry(candidate.Source)
		return false, false
	}
	if created {
		h.Registered++
	}
	return false, false
}

func discoveryProbeAvailable(h *Health, rollouts *CodexRolloutLookup) bool {
	return h.Probes < HeaderProbes && (rollouts == nil || rollouts.probes < HeaderProbes-maxCurrentThreads)
}

func prepareCatalog(store *state.Store, cfg config.Config, o Options, adapter SourceAdapter, h *Health) (catalog, string, []string, error) {
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	var c catalog
	if err := local.Read(path, &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.Errors = append(h.Errors, "catalog_rebuilt")
	}
	roots := approvedRoots(cfg.Discovery.CodexHomes)
	if c.Coverage != nil && c.Coverage.Version != 1 {
		return c, path, roots, errors.New("native coverage requires a newer writer")
	}
	if catalogNeedsReset(c, roots) {
		c = catalog{}
	}
	if o.Rollouts != nil {
		if o.Rollouts.coverage != nil && o.Rollouts.coverage.validate(roots) == nil {
			c.Coverage = o.Rollouts.coverage
		}
		if c.Coverage == nil || c.Coverage.validate(roots) != nil {
			c.Coverage = newCoverage(roots)
		}
		o.Rollouts.coverage = c.Coverage
		if c.Coverage.Phase == "complete" {
			c.Coverage.restart()
			c.Queue = nil
		}
	}
	c.Version = catalogVersion
	c.Roots = roots
	if c.Cache == nil {
		c.Cache = map[string]cached{}
	}
	h.LastReconciled = c.Health.LastReconciled
	if len(c.Queue) == 0 && (c.Coverage == nil || c.Coverage.Phase == "observe") {
		for _, root := range roots {
			for _, path := range adapter.InitialDirectories() {
				c.Queue = append(c.Queue, directory{Root: root, Path: path})
			}
		}
	}
	return c, path, roots, nil
}

func (s scan) observeDirectories(o Options, deadline time.Time) {
	c, h := s.catalog, s.health
	// Retry unavailable directories before the remaining backlog next pass,
	// without revisiting them in this pass or consuming forward queue slots.
	var unavailable []directory
	for (c.Coverage == nil || c.Coverage.Phase == "observe") && len(c.Queue) > 0 && discoveryProbeAvailable(h, o.Rollouts) && h.Entries < 2048 && time.Now().Before(deadline) {
		if scanStopped(s.ctx, o) {
			break
		}
		d := c.Queue[0]
		c.Queue = c.Queue[1:]
		batch, err := s.adapter.Enumerate(s.ctx, d.Root, d.Path, d.Offset)
		next, finished := batch.Continuation, batch.Complete
		if err != nil {
			if s.ctx.Err() != nil {
				c.Queue = append(c.Queue, d)
				break
			}
			h.Errors = appendUnique(h.Errors, "source_root_unavailable")
			unavailable = append(unavailable, d)
			continue
		}
		worker := scan{resolver: s.resolver, store: s.store, cfg: s.cfg, catalog: c, health: h, now: s.now, adapter: s.adapter, ctx: s.ctx, reservedDirectories: len(unavailable), rollouts: o.Rollouts}
		advanced := true
		for _, source := range batch.Entries {
			if scanStopped(s.ctx, o) || time.Now().After(deadline) {
				finished = false
				next = d.Offset
				advanced = false
				break
			}
			retry, stop := worker.visitEntry(d, source)
			if retry {
				finished = false
				next = d.Offset
				advanced = false
			}
			if stop {
				break
			}
		}

		if c.Coverage != nil {
			if batch.coverage == nil {
				c.Coverage.Failed = true
			} else {
				c.Coverage.recordBatch(d, *batch.coverage, next, finished, advanced)
			}
		}
		if !finished {
			d.Offset = next
			c.Queue = append(c.Queue, d)
		}
	}
	c.Queue = append(unavailable, c.Queue...)
	if c.Coverage != nil {
		if c.Coverage.Phase == "observe" && len(c.Queue) == 0 {
			c.Coverage.beginValidation()
		}
		advanceCoverageValidation(s.ctx, c, h, s.adapter, deadline, o)
	}
}
