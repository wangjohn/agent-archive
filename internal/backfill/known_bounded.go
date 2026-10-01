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
	var result KnownProjectsResult
	if ctx.Err() != nil {
		result.TimedOut = true
		return result
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
	seen := map[string]bool{}
	files := 0
	entriesExamined := 0
	r := newResolver(env, cfg, Filters{})
	stopped := func() bool {
		if ctx.Err() != nil {
			result.TimedOut = true
			return true
		}
		return result.Capped
	}
	var walk func(string, bool, int)
	walk = func(dir string, codex bool, depth int) {
		if stopped() {
			return
		}
		entries, err := readDirIfExists(env, dir)
		if err != nil {
			result.Unreadable++
			return
		}
		if stopped() {
			return
		}
		for _, entry := range entries {
			if stopped() {
				return
			}
			entriesExamined++
			if entriesExamined > 8192 {
				result.Capped = true
				return
			}
			path := filepath.Join(dir, entry.name)
			if entry.dir {
				if depth >= 32 {
					result.Capped = true
					return
				}
				walk(path, codex, depth+1)
				continue
			}
			if !entry.regular || !strings.HasSuffix(entry.name, ".jsonl") || (codex && !isRolloutName(entry.name)) {
				continue
			}
			files++
			if files > 4096 {
				result.Capped = true
				return
			}
			if _, ok := fileSize(env, path); !ok {
				result.Unreadable++
				continue
			}
			if stopped() {
				return
			}
			cwd, err := firstProjectRecord(ctx, env, path, codex)
			if err != nil {
				if ctx.Err() != nil {
					result.TimedOut = true
					return
				}
				result.Unreadable++
				continue
			}
			if cwd == "" {
				continue
			}
			if stopped() {
				return
			}
			res := r.resolve(cwd)
			if stopped() {
				return
			}
			if res.skip != "" || res.root == "" || res.kind == ProjectKindHome || res.kind == ProjectKindTemporary || !env.exists(res.root) {
				continue
			}
			root := env.resolved(res.root)
			if seen[root] {
				continue
			}
			if len(result.Projects) >= maxRoots {
				result.Capped = true
				return
			}
			seen[root] = true
			result.Projects = append(result.Projects, KnownProject{Root: root, Kind: res.kind})
		}
	}
	for _, dir := range env.claudeDirs() {
		walk(filepath.Join(dir, "projects"), false, 0)
	}
	for _, dir := range env.codexDirs() {
		walk(filepath.Join(dir, "sessions"), true, 0)
		walk(filepath.Join(dir, "archived_sessions"), true, 0)
	}
	return result
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
