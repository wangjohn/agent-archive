package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestProjectMatcherDeduplicatesAndDoesNotExpandExclusions(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(home, "repo")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	key := archive.RepoKey("https://user:synthetic-secret@example.test/acme/repo.git")
	var calls atomic.Int32
	env := Env{WorkingDir: func() (string, error) { return alias, nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(ctx context.Context, path string) string {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 250*time.Millisecond {
			t.Error("missing child deadline")
		}
		return key
	}}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: root, Included: true}}}}
	got := matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key}})
	if got.Incomplete || len(got.Roots[0]) != 1 || calls.Load() != 1 {
		t.Fatalf("got %+v; calls %d", got, calls.Load())
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: filepath.Join(root, "private"), Included: false})
	got = matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key}})
	if len(got.Roots[0]) != 0 {
		t.Fatalf("exclusion broadened: %+v", got)
	}
	command := anotherMachineCommand(cfg, home, env)
	if !strings.Contains(command, "--project-repo "+key) || strings.Contains(command, "synthetic-secret") || strings.Contains(command, "https://") {
		t.Fatalf("command %q", command)
	}
}

func TestProjectMatcherAmbiguousClonesAndKnownMismatch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	key := archive.RepoKey("git@example.test:acme/repo.git")
	other := archive.RepoKey("git@example.test:acme/other.git")
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	for _, root := range []string{a, b} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := Env{WorkingDir: func() (string, error) { return a, nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(context.Context, string) string { return key }}
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: b, Included: true}}}}
	got := matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key}, {RepoKey: other, Path: a}})
	if got.Incomplete || len(got.Roots[0]) != 2 || len(got.Roots[1]) != 0 {
		t.Fatalf("got %+v", got)
	}
	env.repoKeyContext = func(context.Context, string) string { return "" }
	got = matchProjects(context.Background(), env, home, cfg, []projectMatchRequest{{RepoKey: key, Path: a}})
	if len(got.Roots[0]) != 1 {
		t.Fatalf("no-origin path hint: %+v", got)
	}
}

func TestProjectMatcherStopsAtSharedDeadlineAndBoundsGitConcurrency(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := config.Config{}
	for i := range 8 {
		root := filepath.Join(home, string(rune('a'+i)))
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: root, Included: true})
	}
	var active, peak atomic.Int32
	env := Env{WorkingDir: func() (string, error) { return "", nil }, LookupEnv: func(string) (string, bool) { return "", false }, repoKeyContext: func(ctx context.Context, _ string) string {
		n := active.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-ctx.Done()
		active.Add(-1)
		return ""
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := matchProjects(ctx, env, home, cfg, []projectMatchRequest{{RepoKey: archive.RepoKey("git@example.test:repo.git")}})
	if !got.Incomplete || peak.Load() > 4 || active.Load() != 0 {
		t.Fatalf("got %+v peak %d active %d", got, peak.Load(), active.Load())
	}
}
