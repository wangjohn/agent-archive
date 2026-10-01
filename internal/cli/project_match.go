package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// projectMatchRequest also supports pairing's home-relative hint. A known
// repository mismatch never falls back to the hint.
type projectMatchRequest struct{ RepoKey, Path string }
type projectMatchResult struct {
	Roots      [][]string
	History    backfill.KnownProjectsResult
	Incomplete bool
	TimedOut   bool
	Capped     bool
}

func (e Env) projectRepoKey(ctx context.Context, root string) string {
	var key string
	if e.repoKeyContext != nil {
		key = e.repoKeyContext(ctx, root)
	} else if e.repoKey != nil {
		key = e.repoKey(root)
	} else {
		key = gitremote.RepoKey(ctx, root, nil)
	}
	if !archive.IsRepoKey(key) {
		return ""
	}
	return key
}

// matchProjects shares one deadline across discovery and four bounded Git
// workers. Paths are canonicalized and each repository is queried once.
func matchProjects(ctx context.Context, env Env, userHome string, cfg config.Config, requests []projectMatchRequest) projectMatchResult {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result := projectMatchResult{Roots: make([][]string, len(requests))}
	bf := env.backfillEnvironment(userHome, cfg)
	var candidates []string
	seen := map[string]bool{}
	add := func(path string) {
		if ctx.Err() != nil {
			result.Incomplete = true
			result.TimedOut = true
			return
		}
		if path == "" || !filepath.IsAbs(path) {
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return
		}
		root := path
		if canonical, err := filepath.EvalSymlinks(path); err == nil {
			root = canonical
		}
		root = filepath.Clean(root)
		if seen[root] {
			return
		}
		if len(candidates) >= 128 {
			result.Capped = true
			result.Incomplete = true
			return
		}
		seen[root] = true
		candidates = append(candidates, root)
	}
	if cwd, ok := workingDirOrNone(env); ok {
		add(cwd)
	}
	for _, project := range cfg.Archive.Projects {
		add(project.Root)
	}
	for _, request := range requests {
		add(request.Path)
	}
	result.History = backfill.KnownProjectsBounded(ctx, bf, cfg, 128)
	for _, project := range result.History.Projects {
		add(project.Root)
	}
	result.Incomplete = result.Incomplete || result.History.Incomplete()
	keys := make([]string, len(candidates))
	var mu sync.Mutex
	next := 0
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= len(candidates) || ctx.Err() != nil {
					return
				}
				child, done := context.WithTimeout(ctx, 250*time.Millisecond)
				keys[i] = env.projectRepoKey(child, candidates[i])
				if child.Err() != nil {
					mu.Lock()
					result.Incomplete = true
					result.TimedOut = true
					mu.Unlock()
				}
				done()
			}
		})
	}
	wg.Wait()
	result.TimedOut = result.TimedOut || ctx.Err() != nil || result.History.TimedOut
	result.Capped = result.Capped || result.History.Capped
	result.Incomplete = result.Incomplete || ctx.Err() != nil
	for i, request := range requests {
		for j, root := range candidates {
			if request.RepoKey != "" && keys[j] != request.RepoKey && !(keys[j] == "" && request.Path != "" && filepath.Clean(request.Path) == root) {
				continue
			}
			if request.RepoKey == "" && filepath.Clean(request.Path) != root {
				continue
			}
			blocked := false
			for _, project := range cfg.Archive.Projects {
				if !project.Included && (local.PathWithin(root, project.Root) || local.PathWithin(project.Root, root)) {
					blocked = true
				}
			}
			if !blocked {
				result.Roots[i] = append(result.Roots[i], root)
			}
		}
	}
	return result
}

func printProjectMatches(p *prompter, keys []string, result projectMatchResult) {
	for i, key := range keys {
		reason := "not found"
		if result.Incomplete {
			reason = "incomplete discovery"
		} else if i < len(result.Roots) {
			switch len(result.Roots[i]) {
			case 1:
				continue
			case 0:
			default:
				reason = "multiple clones"
			}
		}
		terminal.Printf(p.out, "Skipped repository %s: %s; use --project DIR to choose a local path.\n", key, reason)
	}
	if result.Incomplete {
		terminal.Printf(p.out, "Project discovery: timeout=%t, cap=%t, unreadable=%d.\n", result.TimedOut, result.Capped, result.History.Unreadable)
	}
}
