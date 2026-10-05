package gitremote

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var commitSHA = strings.Repeat("3f", 20)

// scriptedRunner answers git by its subcommand (the argument after -C DIR),
// and records what it was asked.
type scriptedRunner struct {
	mu      sync.Mutex
	answers map[string]func() ([]byte, error)
	calls   [][]string
}

func (r *scriptedRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	answer := r.answers[args[2]]
	r.mu.Unlock()
	if answer == nil {
		return nil, errors.New("exit status 128")
	}
	return answer()
}

func answer(out string, err error) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(out), err }
}

func TestHeadAsksGitForTheCommitInTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := &scriptedRunner{answers: map[string]func() ([]byte, error){"rev-parse": answer(commitSHA+"\n", nil)}}
	if got := Head(t.Context(), dir, fake.run); got != commitSHA {
		t.Fatalf("Head = %q, want %q", got, commitSHA)
	}
	want := []string{"-C", dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"}
	if len(fake.calls) != 1 || !slices.Equal(fake.calls[0], want) {
		t.Errorf("git ran as %v, want %v", fake.calls, want)
	}
}

func TestHeadIsEmptyUnlessGitNamesACommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, reply := range map[string]func() ([]byte, error){
		"git is not installed":       answer("", exec.ErrNotFound),
		"not a repository":           answer("", errors.New("exit status 128")),
		"a branch with no commits":   answer("", errors.New("exit status 1")),
		"an abbreviation":            answer("3f9c2ab\n", nil),
		"a symbolic name":            answer("HEAD\n", nil),
		"two lines":                  answer(commitSHA+"\n"+commitSHA+"\n", nil),
		"output past the cap":        answer(commitSHA, ErrOutputLimit),
		"upper case":                 answer(strings.ToUpper(commitSHA), nil),
		"a SHA-1 with trailing text": answer(commitSHA+" HEAD", nil),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &scriptedRunner{answers: map[string]func() ([]byte, error){"rev-parse": reply}}
			if got := Head(t.Context(), dir, fake.run); got != "" {
				t.Errorf("Head = %q, want empty", got)
			}
		})
	}
	sha256Name := strings.Repeat("ab", 32)
	fake := &scriptedRunner{answers: map[string]func() ([]byte, error){"rev-parse": answer(sha256Name, nil)}}
	if got := Head(t.Context(), dir, fake.run); got != sha256Name {
		t.Errorf("Head = %q for a SHA-256 repository", got)
	}
	for _, relative := range []string{"", ".", "widget"} {
		if got := Head(t.Context(), relative, fake.run); got != "" {
			t.Errorf("Head(%q) = %q, want empty for a relative directory", relative, got)
		}
	}
}

func TestDirtyReadsOnlyWhetherGitStatusPrintedAnything(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		reply func() ([]byte, error)
		want  *bool
	}{
		"a clean tree":           {answer("", nil), new(false)},
		"a changed file":         {answer(" M widget.go\n", nil), new(true)},
		"an untracked file":      {answer("?? notes.txt\n", nil), new(true)},
		"more than the cap":      {answer(strings.Repeat("?? f\n", 10), ErrOutputLimit), new(true)},
		"not a repository":       {answer("", errors.New("exit status 128")), nil},
		"git is not installed":   {answer("", exec.ErrNotFound), nil},
		"the cap with no output": {answer("", ErrOutputLimit), nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := &scriptedRunner{answers: map[string]func() ([]byte, error){"status": tc.reply}}
			got := Dirty(t.Context(), dir, fake.run)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("Dirty = %v, want %v", deref(got), deref(tc.want))
			}
			want := []string{"-C", dir, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=dirty"}
			if len(fake.calls) != 1 || !slices.Equal(fake.calls[0], want) {
				t.Errorf("git ran as %v, want %v", fake.calls, want)
			}
		})
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func TestHeadStateAsksForDirtyOnlyWhenTold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := &scriptedRunner{answers: map[string]func() ([]byte, error){
		"rev-parse": answer(commitSHA, nil),
		"status":    answer(" M widget.go", nil),
	}}
	if sha, dirty := HeadState(dir, false, fake.run); sha != commitSHA || dirty != nil || len(fake.calls) != 1 {
		t.Errorf("HeadState without dirty = %q, %v after %v", sha, deref(dirty), fake.calls)
	}
	if sha, dirty := HeadState(dir, true, fake.run); sha != commitSHA || dirty == nil || !*dirty {
		t.Errorf("HeadState with dirty = %q, %v", sha, deref(dirty))
	}
	// Without a commit, a dirty flag is relative to nothing.
	noCommit := &scriptedRunner{answers: map[string]func() ([]byte, error){"status": answer("?? notes.txt", nil)}}
	if sha, dirty := HeadState(dir, true, noCommit.run); sha != "" || dirty != nil {
		t.Errorf("HeadState without a commit = %q, %v", sha, deref(dirty))
	}
}

// Head and Dirty run at the same time, so a slow status does not add to the
// commit's time: each waits for the other to start.
func TestHeadStateAsksBothAtOnce(t *testing.T) {
	t.Parallel()
	headStarted, statusStarted := make(chan struct{}), make(chan struct{})
	waitFor := func(started chan struct{}) bool {
		select {
		case <-started:
			return true
		case <-time.After(Timeout / 2):
			return false
		}
	}
	fake := &scriptedRunner{answers: map[string]func() ([]byte, error){
		"rev-parse": func() ([]byte, error) {
			close(headStarted)
			if !waitFor(statusStarted) {
				return nil, errors.New("status never started")
			}
			return []byte(commitSHA), nil
		},
		"status": func() ([]byte, error) {
			close(statusStarted)
			if !waitFor(headStarted) {
				return nil, errors.New("rev-parse never started")
			}
			return nil, nil
		},
	}}
	if sha, dirty := HeadState(t.TempDir(), true, fake.run); sha != commitSHA || dirty == nil || *dirty {
		t.Errorf("HeadState = %q, %v; want the commit, clean, from lookups run together", sha, deref(dirty))
	}
}

func TestHeadReturnsInBoundedTimeWhenGitHangs(t *testing.T) {
	fakeGitOnPath(t, `exec sleep 30`)
	start := time.Now()
	sha, dirty := HeadState(t.TempDir(), true, nil)
	if sha != "" || dirty != nil {
		t.Errorf("HeadState = %q, %v from a git that never answered", sha, deref(dirty))
	}
	if elapsed := time.Since(start); elapsed > hangSlack {
		t.Errorf("HeadState took %v, want about %v", elapsed, Timeout)
	}
}

// Against the real git, when there is one: a clean checkout, a dirty one, an
// untracked file, a detached HEAD, a linked worktree with its own HEAD, a
// subdirectory, a repository with no commits, and a directory outside any
// repository.
func TestHeadStateWithTheRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// Not parallel: t.Setenv keeps the user's git configuration out of it.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	base := t.TempDir()
	repo := filepath.Join(base, "widget")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(base, "init", "-q", repo)
	if sha, dirty := HeadState(repo, true, nil); sha != "" || dirty != nil {
		t.Errorf("a repository with no commits: %q, %v", sha, deref(dirty))
	}
	writeFile(t, filepath.Join(repo, "src", "widget.go"), "package widget\n")
	git(repo, "add", ".")
	git(repo, "commit", "-q", "-m", "first")
	first := git(repo, "rev-parse", "HEAD")
	check := func(what, dir, wantSHA string, wantDirty bool) {
		t.Helper()
		sha, dirty := HeadState(dir, true, nil)
		if sha != wantSHA || dirty == nil || *dirty != wantDirty {
			t.Errorf("%s: HeadState = %q, %v; want %q, %t", what, sha, deref(dirty), wantSHA, wantDirty)
		}
	}
	check("a clean checkout", repo, first, false)
	check("a subdirectory", filepath.Join(repo, "src"), first, false)
	writeFile(t, filepath.Join(repo, "notes.txt"), "draft\n")
	check("an untracked file", repo, first, true)
	writeFile(t, filepath.Join(repo, ".gitignore"), "notes.txt\n.gitignore\n")
	check("an ignored file", repo, first, false)
	writeFile(t, filepath.Join(repo, "src", "widget.go"), "package widget // edited\n")
	check("an edited file", repo, first, true)
	git(repo, "commit", "-q", "-am", "second")
	second := git(repo, "rev-parse", "HEAD")
	git(repo, "checkout", "-q", "--detach", first)
	check("a detached HEAD", repo, first, false)
	worktree := filepath.Join(base, "widget-feature")
	git(repo, "worktree", "add", "-q", "-b", "feature", worktree, second)
	check("a linked worktree", worktree, second, false)
	check("the main checkout beside it", repo, first, false)
	if sha, dirty := HeadState(t.TempDir(), true, nil); sha != "" || dirty != nil {
		t.Errorf("outside a repository: %q, %v", sha, deref(dirty))
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
