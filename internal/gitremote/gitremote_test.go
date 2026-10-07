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
	// The deadlines are checked against clock readings, not elapsed time, so
	// a slow scheduler (-race on a loaded runner) cannot fail the test.
	var deadline time.Time
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
	// A shorter parent's deadline is the one git gets.
	if want, _ := parent.Deadline(); !deadline.Equal(want) {
		t.Errorf("git's deadline is %v, want the parent's %v", deadline, want)
	}
	// With no shorter parent, git gets Timeout and no more: the deadline is
	// Timeout past some moment between the call and git starting.
	deadline = time.Time{}
	var started time.Time
	fast := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		started = time.Now()
		deadline, _ = ctx.Deadline()
		return nil, errors.New("exit status 1")
	}
	before := time.Now()
	OriginURL(t.Context(), t.TempDir(), fast)
	if deadline.IsZero() {
		t.Fatal("git ran without a deadline")
	}
	if deadline.After(started.Add(Timeout)) {
		t.Errorf("git's deadline is %v after it started, want at most %v", deadline.Sub(started), Timeout)
	}
	if deadline.Before(before.Add(Timeout)) {
		t.Errorf("git's deadline is %v after the call, want at least %v", deadline.Sub(before), Timeout)
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
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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

func TestBranchAsksGitForTheBranchInTheDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := &fakeRunner{out: "feature/x\n"}
	if got := Branch(t.Context(), dir, fake.run); got != "feature/x" {
		t.Fatalf("Branch = %q", got)
	}
	want := []string{"-C", dir, "branch", "--show-current"}
	if len(fake.calls) != 1 || !slices.Equal(fake.calls[0], want) || fake.dirs[0] != dir {
		t.Errorf("git ran as %v in %v, want %v in %s", fake.calls, fake.dirs, want, dir)
	}
}

func TestBranchIsEmptyWhenGitCannotAnswer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, fake := range map[string]*fakeRunner{
		"git is not installed":         {err: exec.ErrNotFound},
		"git too old for the flag":     {err: errors.New("exit status 129")},
		"not a repository":             {err: errors.New("exit status 128")},
		"detached HEAD prints nothing": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := Branch(t.Context(), dir, fake.run); got != "" {
				t.Errorf("Branch = %q, want empty", got)
			}
		})
	}
	fake := &fakeRunner{out: "main"}
	for _, relative := range []string{"", ".", "repo"} {
		if got := Branch(t.Context(), relative, fake.run); got != "" {
			t.Errorf("Branch(%q) = %q, want empty", relative, got)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("git ran for a relative directory: %v", fake.calls)
	}
}

func TestExecRunnerReadsTheBranchOfARepositoryAndOfASubdirectory(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), git, append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// An unborn branch has its name before any commit.
	runGit("symbolic-ref", "HEAD", "refs/heads/topic/one")
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, sub} {
		if got := Branch(t.Context(), dir, nil); got != "topic/one" {
			t.Errorf("Branch(%s) = %q", dir, got)
		}
	}
	// A tag of the same name must not turn the branch into heads/foo.
	runGit("commit", "-q", "--allow-empty", "-m", "first")
	runGit("checkout", "-q", "-b", "foo")
	runGit("tag", "foo")
	if got := Branch(t.Context(), root, nil); got != "foo" {
		t.Errorf("Branch with a tag of the same name = %q, want foo", got)
	}
	// Detached HEAD has no branch.
	runGit("checkout", "-q", "--detach")
	if got := Branch(t.Context(), root, nil); got != "" {
		t.Errorf("Branch of a detached HEAD = %q, want empty", got)
	}
	if got := Branch(t.Context(), t.TempDir(), nil); got != "" {
		t.Errorf("Branch of a plain directory = %q", got)
	}
}

type projectExitStatusError int

func (s projectExitStatusError) Error() string { return "synthetic Git exit" }

func (s projectExitStatusError) ExitCode() int { return int(s) }

func TestProjectKeyDistinguishesMissingOriginFromUnknownIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		out   string
		err   error
		known bool
	}{
		{name: "portable", out: "https://user:synthetic-secret@example.test/acme/repo.git", known: true},
		{name: "no origin", err: projectExitStatusError(1), known: true},
		{name: "empty", known: true},
		{name: "not installed", err: exec.ErrNotFound},
		{name: "repository failure", err: projectExitStatusError(128)},
		{name: "malformed", out: "invalid origin"},
		{name: "nonportable", out: "file:///tmp/source"},
		{name: "failed with output", out: "invalid", err: projectExitStatusError(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{out: tc.out, err: tc.err}
			key, known := ProjectKey(t.Context(), t.TempDir(), fake.run)
			if known != tc.known || key != archive.RepoKey(tc.out) || strings.Contains(key, "synthetic-secret") {
				t.Fatalf("key %q known %t", key, known)
			}
		})
	}
}

func TestProjectRootEstablishesCheckoutScopeFromSubdirectory(t *testing.T) {
	git := gitOrSkip(t)
	root := initRepo(t, git, "https://example.test/repo.git")
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "pkg")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, child} {
		if got := ProjectRoot(t.Context(), path, nil); got != root {
			t.Fatalf("ProjectRoot(%q)=%q want %q", path, got, root)
		}
	}
	if got := ProjectRoot(t.Context(), t.TempDir(), nil); got != "" {
		t.Fatalf("plain directory: %q", got)
	}
}

func TestProjectRootWithholdsFailedOrMalformedScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		err  error
	}{
		{name: "failed", out: "/repo", err: projectExitStatusError(128)},
		{name: "relative", out: "repo"},
		{name: "multiple lines", out: "/repo\n/other\n"},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{out: tc.out, err: tc.err}
			if got := ProjectRoot(t.Context(), t.TempDir(), fake.run); got != "" {
				t.Fatalf("unproven checkout: %q", got)
			}
		})
	}
}
