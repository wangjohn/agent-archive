package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// testGit runs the real git with no global or system configuration, so a
// developer's settings (hooks, signing, default branch) cannot change what
// these tests see. It is Env.RunGit for them.
func testGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return out, nil
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := testGit(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// newTestRepo makes a repository with one commit, in a directory of its own
// so the worktree created beside it stays inside the test's temp directory.
// It returns the checkout's top level with symlinks resolved, as git reports
// it.
func newTestRepo(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "app")
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "one\n", 0o644)
	writeTestFile(t, filepath.Join(repo, "staged.txt"), "one\n", 0o644)
	writeTestFile(t, filepath.Join(repo, "sub", "inner", "keep.txt"), "keep\n", 0o644)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), ".env\nbuild/\n", 0o644)
	initTestRepo(t, repo)
	return repo
}

// initTestRepo commits everything in dir as a new repository's first commit.
func initTestRepo(t *testing.T, dir string) {
	t.Helper()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "initial")
}

// resolvedPath is path with symlinks resolved, as git reports a top level
// (macOS's /var is /private/var).
func resolvedPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// worktreeEnv is an Env whose git is testGit and whose clock and home are
// fixed; nothing is a terminal.
func worktreeEnv(t *testing.T, home string, now time.Time) Env {
	t.Helper()
	env := testEnv(t, home, now)
	env.RunGit = testGit
	env.IsTerminal = func(any) bool { return false }
	return env
}

func worktreeTarget(id string) handoffTarget {
	return handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: id}, source: "archive"}
}

func TestHandoffWorktreeFromCleanRepository(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	env := worktreeEnv(t, t.TempDir(), time.Now())
	var stderr bytes.Buffer
	dir, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("abcdef1234567890"), repo, nil, nil, &stderr)
	want := repo + "-handoff-abcdef12"
	if err != nil || dir != want {
		t.Fatalf("dir=%q err=%v, want %s", dir, err, want)
	}
	if got := strings.TrimSpace(mustGit(t, dir, "branch", "--show-current")); got != "handoff/abcdef12" {
		t.Fatalf("worktree branch = %q", got)
	}
	if got := strings.TrimSpace(mustGit(t, dir, "status", "--porcelain")); got != "" {
		t.Fatalf("clean handoff worktree has changes: %q", got)
	}
	if line := fmt.Sprintf("handoff: created worktree %s on branch handoff/abcdef12 (carried 0 changed and 0 untracked files)\n", want); stderr.String() != line {
		t.Fatalf("stderr = %q, want %q", stderr.String(), line)
	}
}

func TestHandoffWorktreeCarriesChangesAndLeavesTheCheckoutAlone(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "two\n", 0o644)
	writeTestFile(t, filepath.Join(repo, "staged.txt"), "staged\n", 0o644)
	writeTestFile(t, filepath.Join(repo, "added.txt"), "added\n", 0o644)
	mustGit(t, repo, "add", "staged.txt", "added.txt")
	writeTestFile(t, filepath.Join(repo, "notes", "run.sh"), "#!/bin/sh\n", 0o775)
	writeTestFile(t, filepath.Join(repo, ".env"), "SECRET=1\n", 0o600)
	writeTestFile(t, filepath.Join(repo, "build", "out.bin"), "bin\n", 0o644)
	if err := os.Symlink("/etc/hosts", filepath.Join(repo, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "inside-link")); err != nil {
		t.Fatal(err)
	}
	statusBefore := mustGit(t, repo, "status", "--porcelain")
	env := worktreeEnv(t, t.TempDir(), time.Now())
	var stderr bytes.Buffer
	dir, err := prepareLaunchDir(env, handoffOptions{to: "codex", worktree: true, branch: "feature/carry"}, worktreeTarget("s1"), repo, nil, nil, &stderr)
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "on branch feature/carry (carried 3 changed and 3 untracked files)") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	for name, want := range map[string]string{"tracked.txt": "two\n", "staged.txt": "staged\n", "added.txt": "added\n", "notes/run.sh": "#!/bin/sh\n"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != want {
			t.Errorf("%s in worktree = %q, %v; want %q", name, data, err, want)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "notes", "run.sh")); err != nil || info.Mode().Perm() != 0o775 {
		t.Errorf("run.sh mode = %v, %v; want 0775", info, err)
	}
	for _, ignored := range []string{".env", "build"} {
		if _, err := os.Lstat(filepath.Join(dir, ignored)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("ignored %s was copied (%v)", ignored, err)
		}
	}
	for link, target := range map[string]string{"outside-link": "/etc/hosts", "inside-link": "tracked.txt"} {
		info, err := os.Lstat(filepath.Join(dir, link))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink in the worktree: %v, %v", link, info, err)
		}
		if got, _ := os.Readlink(filepath.Join(dir, link)); got != target {
			t.Errorf("%s -> %q, want %q", link, got, target)
		}
	}
	// The original checkout and the stash stack are untouched.
	if after := mustGit(t, repo, "status", "--porcelain"); after != statusBefore {
		t.Fatalf("checkout status changed:\n%s\nwas\n%s", after, statusBefore)
	}
	if list := mustGit(t, repo, "stash", "list"); list != "" {
		t.Fatalf("stash list = %q", list)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "tracked.txt")); string(data) != "two\n" {
		t.Fatalf("original tracked.txt = %q", data)
	}
}

func TestHandoffWorktreeLaunchesInTheMatchingSubdirectory(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	env := worktreeEnv(t, t.TempDir(), time.Now())
	dir, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("sub12345"), filepath.Join(repo, "sub", "inner"), nil, nil, io.Discard)
	if want := filepath.Join(repo+"-handoff-sub12345", "sub", "inner"); err != nil || dir != want {
		t.Fatalf("dir=%q err=%v, want %s", dir, err, want)
	}
	// A directory the worktree does not have (ignored here) starts at its
	// top level.
	if err := os.MkdirAll(filepath.Join(repo, "build", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	dir, err = prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("bld12345"), filepath.Join(repo, "build", "x"), nil, nil, &stderr)
	if want := repo + "-handoff-bld12345"; err != nil || dir != want || !strings.Contains(stderr.String(), "starting at its top level") {
		t.Fatalf("dir=%q err=%v stderr=%q, want %s", dir, err, stderr.String(), want)
	}
}

func TestHandoffWorktreeRefusesExistingBranchOrDirectory(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	env := worktreeEnv(t, t.TempDir(), time.Now())
	opts := handoffOptions{to: "claude", worktree: true}

	mustGit(t, repo, "branch", "handoff/taken123")
	_, err := prepareLaunchDir(env, opts, worktreeTarget("taken123"), repo, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "branch handoff/taken123 already exists") || !strings.Contains(err.Error(), "--branch") {
		t.Fatalf("existing branch: err=%v", err)
	}
	if _, statErr := os.Lstat(repo + "-handoff-taken123"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree directory created despite the error: %v", statErr)
	}

	// A branch whose name only starts with the default is not a match.
	mustGit(t, repo, "branch", "handoff/pre12345-other")
	if _, err := prepareLaunchDir(env, opts, worktreeTarget("pre12345"), repo, nil, nil, io.Discard); err != nil {
		t.Fatalf("prefix branch: err=%v", err)
	}

	existing := repo + "-handoff-exists12"
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = prepareLaunchDir(env, opts, worktreeTarget("exists12"), repo, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), existing+" already exists") {
		t.Fatalf("existing directory: err=%v", err)
	}

	_, err = prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true, branch: "bad..name"}, worktreeTarget("badname1"), repo, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"bad..name" is not a valid branch name`) {
		t.Fatalf("invalid branch: err=%v", err)
	}
}

func TestHandoffWorktreeNeedsAGitCheckout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	env := worktreeEnv(t, t.TempDir(), time.Now())
	_, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("s1"), dir, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--worktree needs a git checkout") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("err=%v", err)
	}
}

func TestHandoffWorktreeFailureAfterAddLeavesTheWorktree(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "two\n", 0o644)
	env := worktreeEnv(t, t.TempDir(), time.Now())
	env.RunGit = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == "stash" && args[1] == "apply" {
			return nil, errors.New("apply failed")
		}
		return testGit(ctx, dir, args...)
	}
	path := repo + "-handoff-fail1234"
	_, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("fail1234"), repo, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "created worktree "+path) || !strings.Contains(err.Error(), "left in place") || !strings.Contains(err.Error(), "apply failed") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(path, "tracked.txt")); statErr != nil {
		t.Fatalf("worktree removed after the failure: %v", statErr)
	}
}

func TestCopyUntrackedRefusesPathsOutsideTheTree(t *testing.T) {
	t.Parallel()
	src, dst := t.TempDir(), t.TempDir()
	for _, name := range []string{"../escape", "/etc/hosts"} {
		if _, _, err := copyUntracked(src, dst, []string{name}); err == nil || !strings.Contains(err.Error(), "outside the checkout") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	// A symlinked directory in the source is never followed to read a file
	// outside it.
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "secret"), "x", 0o600)
	if err := os.Symlink(outside, filepath.Join(src, "dir")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := copyUntracked(src, dst, []string{"dir/secret"}); err == nil {
		t.Fatal("copied a file through a symlink out of the source tree")
	}
}

func TestEnvRunGitReportsGitStderr(t *testing.T) {
	t.Parallel()
	_, err := Env{}.runGit(t.Context(), t.TempDir(), "rev-parse", "--show-toplevel")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("err=%v", err)
	}
}

// activeFixture is a local source session registered for its project, last
// active 30 seconds before the clock.
func activeFixture(t *testing.T) (handoffFixture, handoffTarget) {
	t.Helper()
	f := newHandoffFixture(t, false)
	now := f.env.now()
	f.env.RunGit = testGit
	f.env.IsTerminal = func(any) bool { return false }
	target := handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: f.id, NativeSessionID: "native-1"}, source: "local", lastActivityAt: now.Add(-30 * time.Second)}
	return f, target
}

func TestActiveSourceWarnsOffATerminal(t *testing.T) {
	t.Parallel()
	f, target := activeFixture(t)
	var stderr bytes.Buffer
	dir, err := prepareLaunchDir(f.env, handoffOptions{to: "claude"}, target, f.project, nil, nil, &stderr)
	if err != nil || dir != f.project || !strings.Contains(stderr.String(), "handoff: warning: the source session was active just now in this checkout") {
		t.Fatalf("dir=%q err=%v stderr=%q", dir, err, stderr.String())
	}
	// Idle for longer than the window, in another checkout, or from the
	// archive: no warning.
	for name, change := range map[string]func(*handoffTarget, *string){
		"idle":          func(t *handoffTarget, _ *string) { t.lastActivityAt = t.lastActivityAt.Add(-3 * time.Minute) },
		"clock behind":  func(t *handoffTarget, _ *string) { t.lastActivityAt = t.lastActivityAt.Add(4 * time.Minute) },
		"other project": func(_ *handoffTarget, dir *string) { *dir = filepath.Dir(*dir) },
		"archive":       func(t *handoffTarget, _ *string) { t.source = "archive" },
	} {
		tgt, d := target, f.project
		change(&tgt, &d)
		stderr.Reset()
		if got, err := prepareLaunchDir(f.env, handoffOptions{to: "claude"}, tgt, d, nil, nil, &stderr); err != nil || got != d || stderr.Len() != 0 {
			t.Errorf("%s: dir=%q err=%v stderr=%q", name, got, err, stderr.String())
		}
	}
	// The window is inclusive at two minutes and holds for a clock slightly
	// behind the transcript's.
	for _, offset := range []time.Duration{-activeSourceWindow, 10 * time.Second} {
		tgt := target
		tgt.lastActivityAt = f.env.now().Add(offset)
		stderr.Reset()
		if _, err := prepareLaunchDir(f.env, handoffOptions{to: "claude"}, tgt, f.project, nil, nil, &stderr); err != nil || !strings.Contains(stderr.String(), "warning") {
			t.Errorf("offset %v: err=%v stderr=%q", offset, err, stderr.String())
		}
	}
}

func TestActiveSourceAsksOnATerminal(t *testing.T) {
	t.Parallel()
	f, target := activeFixture(t)
	// Only stdin and stderr are checked for a terminal; the answers come
	// from a reader the command shares between its questions.
	tty := strings.NewReader("")
	f.env.IsTerminal = func(stream any) bool {
		_, isBuffer := stream.(*bytes.Buffer)
		return stream == tty || isBuffer
	}
	repo := f.project
	initTestRepo(t, repo)
	// Only the last answer creates a worktree, so the cases share one repository.
	for _, tc := range []struct {
		answer  string
		wantErr bool
		// worktree is whether the answer launches in a new worktree.
		worktree bool
	}{
		{"y\n", false, false},
		{"yes\n", false, false},
		{"n\n", true, false},
		// A blank answer is N at once, never a reason to ask again.
		{"\ny\n", true, false},
		{"", true, false},
		{"maybe\nw\n", false, true},
	} {
		var stderr bytes.Buffer
		dir, err := prepareLaunchDir(f.env, handoffOptions{to: "claude"}, target, repo, tty, bufio.NewReader(strings.NewReader(tc.answer)), &stderr)
		if !strings.Contains(stderr.String(), "The source session was active just now; continue in the same checkout? [y/N/w]") {
			t.Errorf("%q: no question in %q", tc.answer, stderr.String())
		}
		switch {
		case tc.wantErr:
			if !errors.Is(err, errHandoffCanceled) {
				t.Errorf("%q: err=%v, want canceled", tc.answer, err)
			}
		case tc.worktree:
			if want := resolvedPath(t, repo) + "-handoff-" + handoffShortID(target); err != nil || dir != want {
				t.Errorf("%q: dir=%q err=%v, want %s", tc.answer, dir, err, want)
			}
		default:
			if err != nil || dir != repo {
				t.Errorf("%q: dir=%q err=%v", tc.answer, dir, err)
			}
		}
	}
}

func TestActiveSourceDoesNotAskTheCallingAgent(t *testing.T) {
	t.Parallel()
	f, target := activeFixture(t)
	f.env.IsTerminal = func(any) bool { return true }
	f.env.LookupEnv = func(key string) (string, bool) {
		if key == "CODEX_THREAD_ID" {
			return "native-1", true
		}
		return "", false
	}
	var stderr bytes.Buffer
	dir, err := prepareLaunchDir(f.env, handoffOptions{to: "claude"}, target, f.project, nil, strings.NewReader(""), &stderr)
	if err != nil || dir != f.project || strings.Contains(stderr.String(), "[y/N/w]") || strings.Count(stderr.String(), "\n") != 1 ||
		!strings.Contains(stderr.String(), "is the agent running this command") {
		t.Fatalf("dir=%q err=%v stderr=%q", dir, err, stderr.String())
	}
	// Another agent's session variable does not make it the caller.
	f.env.LookupEnv = func(key string) (string, bool) {
		if key == "CLAUDE_CODE_SESSION_ID" {
			return "native-1", true
		}
		return "", false
	}
	stderr.Reset()
	if _, err := prepareLaunchDir(f.env, handoffOptions{to: "claude"}, target, f.project, nil, strings.NewReader("n\n"), &stderr); !errors.Is(err, errHandoffCanceled) {
		t.Fatalf("other harness's variable: err=%v stderr=%q", err, stderr.String())
	}
}

// The N answer ends the command with exit 1 and launches nothing.
func TestHandoffActiveSourceCancelLaunchesNothing(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	info, err := os.Stat(filepath.Join(f.project, "codex.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f.env.Now = func() time.Time { return info.ModTime().Add(time.Minute) }
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.IsTerminal = func(any) bool { return true }
	f.env.RunGit = testGit
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("launched after the answer N")
		return nil
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"handoff", f.id, "--to", "claude"}, strings.NewReader("n\n"), &out, &errOut, f.env)
	if code != 1 || !strings.Contains(errOut.String(), "[y/N/w]") || !strings.Contains(errOut.String(), "canceled; nothing was launched") {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if entries, _ := os.ReadDir(filepath.Join(f.home, handoffDir)); len(entries) != 0 {
		t.Fatalf("canceled handoff wrote %v", entries)
	}
}

func TestHandoffToWithWorktreeLaunchesInTheWorktree(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	initTestRepo(t, f.project)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.RunGit = testGit
	var got launchSpec
	f.env.LaunchHandoff = func(spec launchSpec, _ io.Reader, _, _ io.Writer) error {
		got = spec
		return nil
	}
	_, errOut, code := runHandoff(t, f.env, f.id, "--to", "claude", "--worktree", "--branch", "try/it")
	want := resolvedPath(t, f.project) + "-handoff-" + handoffShortID(handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: f.id}})
	if code != 0 || got.Dir != want || !strings.Contains(errOut, "on branch try/it") || !strings.Contains(errOut, "launching local claude in "+want) {
		t.Fatalf("code=%d dir=%q stderr=%q, want %s", code, got.Dir, errOut, want)
	}
}

func TestHandoffWorktreeFlagsNeedALaunch(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	for _, args := range [][]string{
		{f.id, "--worktree"},
		{f.id, "--to", "claude", "--branch", "x"},
	} {
		if _, errOut, code := runHandoff(t, f.env, args...); code != 2 || !strings.Contains(errOut, "applies only") {
			t.Errorf("%q: code=%d stderr=%q", args, code, errOut)
		}
	}
}
