package gitremote

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// fakeRunner answers every call with out and err, and records the calls.
type fakeRunner struct {
	out   string
	err   error
	calls [][]string
	dirs  []string
}

func (f *fakeRunner) run(_ context.Context, dir string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	f.dirs = append(f.dirs, dir)
	return []byte(f.out), f.err
}

func TestOriginURLAsksGitForTheOriginInTheProjectRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := &fakeRunner{out: "https://example.test/acme/widget.git\n"}
	if got := OriginURL(t.Context(), root, fake.run); got != "https://example.test/acme/widget.git" {
		t.Fatalf("OriginURL = %q", got)
	}
	want := []string{"-C", root, "config", "--get", "remote.origin.url"}
	if len(fake.calls) != 1 || !slices.Equal(fake.calls[0], want) || fake.dirs[0] != root {
		t.Errorf("git ran as %v in %v, want %v in %s", fake.calls, fake.dirs, want, root)
	}
}

func TestOriginURLIsEmptyWhenGitCannotAnswer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, fake := range map[string]*fakeRunner{
		"git is not installed": {err: exec.ErrNotFound},
		"not a repository":     {err: errors.New("exit status 128")},
		"no origin remote":     {err: errors.New("exit status 1")},
		"empty output":         {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := OriginURL(t.Context(), root, fake.run); got != "" {
				t.Errorf("OriginURL = %q, want empty", got)
			}
			if got := RepoKey(t.Context(), root, fake.run); got != "" {
				t.Errorf("RepoKey = %q, want empty", got)
			}
		})
	}
}

func TestOriginURLIgnoresARelativeRoot(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{out: "https://example.test/acme/widget.git"}
	for _, root := range []string{"", ".", "widget"} {
		if got := OriginURL(t.Context(), root, fake.run); got != "" {
			t.Errorf("OriginURL(%q) = %q, want empty", root, got)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("git ran for a relative root: %v", fake.calls)
	}
}

func TestOriginURLGivesGitAtMostTheTimeout(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	start := time.Now()
	slow := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		<-ctx.Done()
		return []byte("https://example.test/acme/widget.git"), nil
	}
	parent, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if got := OriginURL(parent, t.TempDir(), slow); got != "" {
		t.Errorf("OriginURL after a timeout = %q, want empty", got)
	}
	if time.Since(start) > Timeout {
		t.Errorf("OriginURL took %v, want less than %v", time.Since(start), Timeout)
	}
	// With no shorter parent, git still gets no more than Timeout.
	deadline = time.Time{}
	fast := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return nil, errors.New("exit status 1")
	}
	begin := time.Now()
	OriginURL(t.Context(), t.TempDir(), fast)
	if deadline.IsZero() || deadline.After(begin.Add(Timeout+50*time.Millisecond)) {
		t.Errorf("git's deadline is %v after the start, want at most %v", deadline.Sub(begin), Timeout)
	}
}

func TestResolverAsksGitOncePerRoot(t *testing.T) {
	t.Parallel()
	first, second := t.TempDir(), t.TempDir()
	fake := &fakeRunner{out: "git@example.test:acme/widget.git"}
	r := &Resolver{Run: fake.run}
	want := archive.RepoKey("https://example.test/acme/widget")
	for range 5 {
		if got := r.Key(first); got != want {
			t.Fatalf("Key = %q, want %q", got, want)
		}
	}
	if len(fake.calls) != 1 {
		t.Errorf("git ran %d times for one root, want 1", len(fake.calls))
	}
	r.Key(second)
	if len(fake.calls) != 2 {
		t.Errorf("git ran %d times for two roots, want 2", len(fake.calls))
	}
}

func TestResolverRemembersAFailure(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{err: exec.ErrNotFound}
	r := &Resolver{Run: fake.run}
	root := t.TempDir()
	for range 3 {
		if got := r.Key(root); got != "" {
			t.Fatalf("Key = %q, want empty", got)
		}
	}
	if len(fake.calls) != 1 {
		t.Errorf("git ran %d times after failing, want 1", len(fake.calls))
	}
}

func TestEnvironmentDropsGitVariablesAndDisablesPrompts(t *testing.T) {
	t.Parallel()
	got := environment([]string{"PATH=/usr/bin", "GIT_DIR=/elsewhere", "git_work_tree=/x", "HOME=/h", "GIT_CONFIG_GLOBAL=/y"})
	joined := strings.Join(got, "\n")
	for _, banned := range []string{"/elsewhere", "/x", "/y"} {
		if strings.Contains(joined, banned) {
			t.Errorf("environment kept %q: %v", banned, got)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "HOME=/h", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"} {
		if !slices.Contains(got, want) {
			t.Errorf("environment lacks %q: %v", want, got)
		}
	}
}

// The remaining tests run the real git in temporary repositories. They are
// not parallel: they change the environment.

func gitOrSkip(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	return git
}

func initRepo(t *testing.T, git, origin string) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		if origin == "" && args[0] == "remote" {
			continue
		}
		cmd := exec.CommandContext(t.Context(), git, append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

func TestExecRunnerReadsTheOriginOfARepositoryAndOfASubdirectory(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://user:token@example.test/acme/widget.git")
	sub := filepath.Join(root, "pkg", "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := archive.RepoKey("git@example.test:acme/widget.git")
	for _, dir := range []string{root, sub} {
		if got := RepoKey(t.Context(), dir, nil); got != want {
			t.Errorf("RepoKey(%s) = %q, want %q", dir, got, want)
		}
	}
}

func TestExecRunnerIsEmptyWithoutAnOriginOrARepository(t *testing.T) {
	git := gitOrSkip(t)
	if got := OriginURL(t.Context(), initRepo(t, git, ""), nil); got != "" {
		t.Errorf("OriginURL of a repository without origin = %q", got)
	}
	if got := OriginURL(t.Context(), t.TempDir(), nil); got != "" {
		t.Errorf("OriginURL of a plain directory = %q", got)
	}
	if got := OriginURL(t.Context(), filepath.Join(t.TempDir(), "gone"), nil); got != "" {
		t.Errorf("OriginURL of a missing directory = %q", got)
	}
}

func TestExecRunnerIgnoresAStrayGitDir(t *testing.T) {
	git := gitOrSkip(t)
	elsewhere := initRepo(t, git, "https://example.test/other/repo.git")
	root := initRepo(t, git, "https://example.test/acme/widget.git")
	t.Setenv("GIT_DIR", filepath.Join(elsewhere, ".git"))
	if got, want := RepoKey(t.Context(), root, nil), archive.RepoKey("https://example.test/acme/widget"); got != want {
		t.Errorf("RepoKey = %q, want %q (GIT_DIR must not redirect git)", got, want)
	}
}

func TestExecRunnerReportsMissingGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := OriginURL(t.Context(), t.TempDir(), nil); got != "" {
		t.Errorf("OriginURL without git on PATH = %q, want empty", got)
	}
}
