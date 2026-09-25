package backfill

import (
	"io"
	"io/fs"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// documentsWatch wraps an Environment's file system calls and records every
// one that looks inside ~/Documents, which macOS guards with a privacy
// prompt.
type documentsWatch struct {
	mu       sync.Mutex
	docs     string
	touched  []string
	original Environment
}

func watchDocuments(env Environment) (*documentsWatch, Environment) {
	w := &documentsWatch{docs: filepath.Join(env.Home, "Documents"), original: env}
	note := func(path string) {
		if local.PathWithin(path, w.docs) && path != w.docs {
			w.mu.Lock()
			w.touched = append(w.touched, path)
			w.mu.Unlock()
		}
	}
	env.Stat = func(p string) (fs.FileInfo, error) { note(p); return w.original.stat(p) }
	env.Lstat = func(p string) (fs.FileInfo, error) { note(p); return w.original.lstat(p) }
	env.ReadDir = func(p string) ([]fs.DirEntry, error) { note(p); return w.original.readDir(p) }
	env.ReadFile = func(p string) ([]byte, error) { note(p); return w.original.readFile(p) }
	env.Open = func(p string) (io.ReadCloser, error) { note(p); return w.original.open(p) }
	env.EvalSymlinks = func(p string) (string, error) { note(p); return w.original.evalSymlinks(p) }
	return w, env
}

// Regression: phase-1 review follow-up. Every backfill plan resolved the
// symlinks of ~/Documents/Codex (Codex desktop's workspace folder), which
// looks inside Documents and makes macOS ask a terminal without access for
// it. A plan whose sessions and configured projects are all elsewhere now
// never looks inside Documents; a session in a Codex workspace still maps
// to the workspace folder.
func TestPlanLooksInDocumentsOnlyWhenInUse(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	tr.mkdir("home/Documents/Codex/2026-09-20/plan-trip")
	project := tr.repo("home/work/project")
	start := fixedNow.Add(-48 * time.Hour)
	id := "0a9b3c4d-0000-4000-8000-0000000000d1"
	tr.write(filepath.Join("home", ".codex", "sessions", "2026", "09", "20", "rollout-2026-09-20T10-00-00-"+id+".jsonl"), codexTranscript(id, id, project, start))

	watch, env := watchDocuments(tr.env())
	p := plan(t, env, nil, config.Config{Harnesses: []string{"codex"}}, Filters{})
	if c := candidate(t, p, id); c.Skip != "" || c.ProjectRoot != project {
		t.Fatalf("session elsewhere: %+v", c)
	}
	if len(watch.touched) > 0 {
		t.Fatalf("a plan with nothing in Documents looked inside it: %v", watch.touched)
	}

	// A configured project in Documents is access already given: the
	// workspace folder is then resolved as it always was.
	inDocs := tr.mkdir("home/Documents/notes")
	cfg := config.Config{Harnesses: []string{"codex"}, Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: inDocs, Included: true, ActivatedAt: fixedNow.Add(-time.Hour)}}}}
	watch, env = watchDocuments(tr.env())
	newResolver(env, cfg, Filters{})
	if len(watch.touched) == 0 {
		t.Fatal("with a project in Documents the workspace folder was not resolved")
	}
}
