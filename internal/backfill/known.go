package backfill

import (
	"context"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// KnownProject is a project that sessions on this Mac ran in, resolved as a
// plan resolves them.
type KnownProject struct {
	Root string
	Kind ProjectKind
	// LastUsed is when a session in it last changed: the newest transcript's
	// modification time.
	LastUsed time.Time
	Sessions int
}

// KnownProjects lists the projects Claude Code and Codex sessions on this
// Mac ran in, most recently used first, for setup to offer. It lists the
// apps' session folders and reads each transcript's leading records, as a
// plan does, and nothing more: no transcript is filtered and nothing is
// written. Projects cfg already names, included or excluded, are left out,
// as are the home directory, temporary folders, folders that no longer
// exist, and sessions whose project cannot be told.
func KnownProjects(ctx context.Context, env Environment, cfg config.Config) ([]KnownProject, error) {
	found, _ := discover(env)
	var items []*work
	for _, t := range found {
		if t.harness != harnessCursor {
			items = append(items, &work{t: t})
		}
	}
	workers := env.Workers
	if workers <= 0 {
		workers = defaultWorkers()
	}
	if err := forEach(ctx, workers, items, func(w *work) {
		if readHead(env, w.t) != nil {
			w.vanished = true
		}
	}); err != nil {
		return nil, err
	}
	r := newResolver(env, cfg, Filters{})
	byRoot := map[string]*KnownProject{}
	for _, w := range items {
		if w.vanished {
			continue
		}
		res := r.resolve(w.t.cwd)
		if res.skip != "" || res.root == "" || res.included || res.kind == ProjectKindHome || res.kind == ProjectKindTemporary {
			continue
		}
		project := byRoot[res.root]
		if project == nil {
			if !env.exists(res.root) {
				continue
			}
			project = &KnownProject{Root: res.root, Kind: res.kind}
			byRoot[res.root] = project
		}
		project.Sessions++
		if info, err := env.lstat(w.t.path); err == nil && info.ModTime().After(project.LastUsed) {
			project.LastUsed = info.ModTime()
		}
	}
	projects := make([]KnownProject, 0, len(byRoot))
	for _, project := range byRoot {
		projects = append(projects, *project)
	}
	sort.Slice(projects, func(i, j int) bool {
		if !projects[i].LastUsed.Equal(projects[j].LastUsed) {
			return projects[i].LastUsed.After(projects[j].LastUsed)
		}
		return projects[i].Root < projects[j].Root
	})
	return projects, nil
}
