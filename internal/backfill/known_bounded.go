package backfill

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// KnownProjectsResult preserves usable roots when discovery cannot finish.
// Native filesystem operations are cooperative: cancellation is checked between
// operations, but cannot interrupt a filesystem syscall already in progress.
type KnownProjectsResult struct {
	Projects   []KnownProject
	TimedOut   bool
	Capped     bool
	Unreadable int
}

// Incomplete reports whether unseen roots could change repository selection.
func (r KnownProjectsResult) Incomplete() bool { return r.TimedOut || r.Capped || r.Unreadable > 0 }

// KnownProjectsBounded asks native integrations for first-record project
// evidence, never searches transcript bodies, and returns partial results. maxRoots
// caps distinct roots; maxFiles bounds enumeration even in duplicate histories.
// The full backfill discovery API keeps its existing behavior.
func KnownProjectsBounded(ctx context.Context, env Environment, cfg config.Config, maxRoots int) KnownProjectsResult {
	if ctx.Err() != nil {
		return KnownProjectsResult{TimedOut: true}
	}
	// Resolution also reads filesystem metadata. Stop subsequent operations
	// after cancellation, including those within the existing resolver.
	original := env
	env.Stat = projectOperation(ctx, original.stat)
	env.Lstat = projectOperation(ctx, original.lstat)
	env.ReadDir = projectOperation(ctx, original.readDir)
	env.ReadFile = projectOperation(ctx, original.readFile)
	env.Open = projectOperation(ctx, original.open)
	env.EvalSymlinks = projectOperation(ctx, original.evalSymlinks)
	if maxRoots <= 0 {
		maxRoots = 128
	}
	d := projectDiscovery{ctx: ctx, env: env, resolver: newResolver(env, cfg, Filters{}), maxRoots: maxRoots, seen: map[string]bool{}}
	if env.Discovery == nil {
		d.result.Unreadable++
		return d.result
	}
	for _, name := range env.Discovery.DiscoveryAgents() {
		if d.stopped() {
			break
		}
		provider, ok := env.Discovery.LookupDiscovery(name)
		if !ok {
			d.result.Unreadable++
			continue
		}
		host := &projectFiles{d: &d, bases: env.nativeDirectories(name)}
		report, err := provider.Discover(ctx, agentapi.DiscoveryRequest{
			Purpose: agentapi.DiscoveryBoundedProjects, Stage: agentapi.DiscoveryIdentities,
			Locations: agentapi.NativeLocations{UserHome: env.Home, Directories: host.bases},
			Files:     host, MaxFiles: 4096 - d.files, HeaderBytes: headLineLimit + 1, RecordBytes: headLineLimit,
			Scan: func(path string, visit func([]byte) bool) error { return firstProjectRecord(ctx, env, path, visit) },
		}, func(c agentapi.DiscoveryCandidate) error {
			if d.stopped() {
				return errProjectLimit
			}
			d.files++
			if c.IdentityError == nil {
				d.addProject(c.Header.Directory, c.Source.Path)
				if d.stopped() {
					return errProjectLimit
				}
			} else {
				d.result.Unreadable++
			}
			return nil
		})
		d.result.Capped = d.result.Capped || host.truncated
		d.result.Unreadable += report.UnreadableFolders
		if report.StoreUnreadable {
			d.result.Unreadable++
		}
		if report.Incomplete && !d.result.Incomplete() {
			d.result.Capped = true
		}
		if err != nil && !errors.Is(err, errProjectLimit) && ctx.Err() == nil {
			d.result.Unreadable++
		}
		if d.files >= 4096 {
			d.result.Capped = true
		}
	}
	d.stopped()
	sort.Slice(d.result.Projects, func(i, j int) bool {
		a, b := d.result.Projects[i], d.result.Projects[j]
		if !a.LastUsed.Equal(b.LastUsed) {
			return a.LastUsed.After(b.LastUsed)
		}
		return a.Root < b.Root
	})
	return d.result
}

type projectDiscovery struct {
	ctx      context.Context
	env      Environment
	resolver *resolver
	result   KnownProjectsResult
	seen     map[string]bool
	maxRoots int
	files    int
	entries  int
}

func (d *projectDiscovery) stopped() bool {
	if d.ctx.Err() != nil {
		d.result.TimedOut = true
		return true
	}
	return d.result.Capped
}

func (d *projectDiscovery) addProject(cwd, source string) {
	if cwd == "" || d.stopped() {
		return
	}
	res := d.resolver.resolve(cwd)
	if d.stopped() || res.skip != "" || res.root == "" || res.kind == ProjectKindHome || res.kind == ProjectKindTemporary || !d.env.exists(res.root) {
		return
	}
	root := d.env.resolved(res.root)
	if d.stopped() {
		return
	}
	if d.seen[root] {
		for i := range d.result.Projects {
			if d.result.Projects[i].Root == root {
				d.recordSession(&d.result.Projects[i], source)
				break
			}
		}
		return
	}
	if len(d.result.Projects) >= d.maxRoots {
		d.result.Capped = true
		return
	}
	d.seen[root] = true
	project := KnownProject{Root: root, Kind: res.kind}
	d.recordSession(&project, source)
	d.result.Projects = append(d.result.Projects, project)
}

func (d *projectDiscovery) recordSession(project *KnownProject, source string) {
	project.Sessions++
	info, err := d.env.lstat(source)
	if err != nil {
		if d.ctx.Err() == nil {
			d.result.Unreadable++
		}
		return
	}
	if info.ModTime().After(project.LastUsed) {
		project.LastUsed = info.ModTime()
	}
}

func projectOperation[T any](ctx context.Context, operation func(string) (T, error)) func(string) (T, error) {
	return func(path string) (T, error) {
		if err := ctx.Err(); err != nil {
			var zero T
			return zero, err
		}
		return operation(path)
	}
}

func firstProjectRecord(ctx context.Context, env Environment, path string, visit func([]byte) bool) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := env.open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	reader := bufio.NewReaderSize(io.LimitReader(f, headLineLimit+1), 4096)
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > headLineLimit {
			return errors.New("project header exceeds limit")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		break
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	visit(line)
	return nil
}

var errProjectLimit = errors.New("bounded project discovery stopped")

// projectFiles bounds entry conversion and traversal before native enumeration.
type projectFiles struct {
	d         *projectDiscovery
	bases     []string
	truncated bool
}

func (p *projectFiles) ReadDir(path string) ([]fs.DirEntry, error) {
	if p.d.stopped() || p.d.entries >= 8192 {
		p.d.result.Capped = true
		return nil, errProjectLimit
	}
	// Each configured native root starts its own bounded traversal. Unrelated
	// roots do not constrain this path, and a nested root gives it a fresh budget.
	depth := -1
	for _, base := range p.bases {
		rel, err := filepath.Rel(base, path)
		if err != nil || !local.PathWithin(path, base) {
			continue
		}
		n := len(strings.Split(rel, string(filepath.Separator)))
		if depth < 0 || n < depth {
			depth = n
		}
	}
	if depth > 33 {
		p.d.result.Capped = true
		return nil, errProjectLimit
	}
	entries, err := p.d.env.readDir(path)
	if p.d.stopped() {
		return nil, p.d.ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	capacity := min(len(entries), 8192-p.d.entries)
	out := make([]fs.DirEntry, 0, capacity)
	wrapped := make([]projectEntry, capacity)
	for _, entry := range entries {
		if p.d.stopped() {
			return nil, p.d.ctx.Err()
		}
		if p.d.entries >= 8192 {
			p.truncated = true
			break
		}
		p.d.entries++
		name := entry.Name()
		if p.d.stopped() {
			return nil, p.d.ctx.Err()
		}
		wrapped[len(out)] = projectEntry{DirEntry: entry, name: name}
		out = append(out, &wrapped[len(out)])
	}
	return out, nil
}

func (p *projectFiles) Lstat(path string) (fs.FileInfo, error) { return p.d.env.lstat(path) }

func (p *projectFiles) Open(path string) (io.ReadCloser, error) { return p.d.env.open(path) }

type projectEntry struct {
	fs.DirEntry
	name string
}

func (e projectEntry) Name() string { return e.name }
