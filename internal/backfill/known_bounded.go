package backfill

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
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

// KnownProjectsBounded reads only the first record of Claude and Codex
// transcripts, never searches their bodies, and returns partial results. maxRoots
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
	for _, dir := range env.nativeDirectories("claude") {
		d.walk(filepath.Join(dir, "projects"), false, 0)
	}
	for _, dir := range env.nativeDirectories("codex") {
		d.walk(filepath.Join(dir, "sessions"), true, 0)
		d.walk(filepath.Join(dir, "archived_sessions"), true, 0)
	}
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

func (d *projectDiscovery) walk(dir string, codex bool, depth int) {
	if d.stopped() {
		return
	}
	// ReadDir itself is a native syscall boundary. Process its entries one
	// at a time so neither conversion nor sorting bypasses our budget.
	entries, err := d.env.readDir(dir)
	if d.stopped() {
		return
	}
	if err != nil {
		if !isNotExist(err) {
			d.result.Unreadable++
		}
		return
	}
	for _, entry := range entries {
		if d.stopped() {
			return
		}
		d.entries++
		if d.entries > 8192 {
			d.result.Capped = true
			return
		}
		path := filepath.Join(dir, entry.Name())
		if d.stopped() {
			return
		}
		if entry.IsDir() {
			if depth >= 32 {
				d.result.Capped = true
				return
			}
			d.walk(path, codex, depth+1)
			continue
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(path, ".jsonl") || (codex && !strings.HasPrefix(entry.Name(), "rollout-")) {
			continue
		}
		d.readProject(path, codex)
	}
}

func (d *projectDiscovery) readProject(path string, codex bool) {
	d.files++
	if d.files > 4096 {
		d.result.Capped = true
		return
	}
	if info, err := d.env.lstat(path); err != nil || !info.Mode().IsRegular() {
		d.result.Unreadable++
		return
	}
	if d.stopped() {
		return
	}
	cwd, err := firstProjectRecord(d.ctx, d.env, path, codex)
	if err != nil {
		if !d.stopped() {
			d.result.Unreadable++
		}
		return
	}
	if cwd == "" || d.stopped() {
		return
	}
	res := d.resolver.resolve(cwd)
	if d.stopped() || res.skip != "" || res.root == "" || res.kind == ProjectKindHome || res.kind == ProjectKindTemporary || !d.env.exists(res.root) {
		return
	}
	root := d.env.resolved(res.root)
	if d.stopped() || d.seen[root] {
		return
	}
	if len(d.result.Projects) >= d.maxRoots {
		d.result.Capped = true
		return
	}
	d.seen[root] = true
	d.result.Projects = append(d.result.Projects, KnownProject{Root: root, Kind: res.kind})
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

func firstProjectRecord(ctx context.Context, env Environment, path string, codex bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := env.open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	reader := bufio.NewReaderSize(io.LimitReader(f, headLineLimit+1), 4096)
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > headLineLimit {
			return "", errors.New("project header exceeds limit")
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		break
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var record struct {
		Cwd     string `json:"cwd"`
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return "", err
	}
	if codex {
		if record.Type == "session_meta" {
			return record.Payload.Cwd, nil
		}
		return "", nil
	}
	return record.Cwd, nil
}
