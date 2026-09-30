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
	"github.com/wangjohn/agent-archive/internal/termlaunch"
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
	// On a terminal, so the agent runs here rather than in a new window.
	f.env.IsTerminal = func(any) bool { return true }
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
		// Off a terminal nothing offers a destination, so --worktree needs --to.
		if _, errOut, code := runHandoff(t, f.env, args...); code != 2 || !strings.Contains(errOut, "appl") {
			t.Errorf("%q: code=%d stderr=%q", args, code, errOut)
		}
	}
}

// Staged-only changes, deletions, and renames are carried, and neither the
// checkout's index nor the shared stash stack changes.
func TestHandoffWorktreeLeavesTheIndexAndStashStackAlone(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	// Someone else's stash entry must stay where it is.
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "stashed\n", 0o644)
	mustGit(t, repo, "stash", "push", "-q", "-m", "theirs")
	writeTestFile(t, filepath.Join(repo, "staged.txt"), "staged only\n", 0o644)
	mustGit(t, repo, "add", "staged.txt")
	mustGit(t, repo, "mv", "tracked.txt", "moved.txt")
	mustGit(t, repo, "rm", "-q", "sub/inner/keep.txt")
	state := func() [3]string {
		return [3]string{mustGit(t, repo, "status", "--porcelain"), mustGit(t, repo, "ls-files", "-s"), mustGit(t, repo, "stash", "list", "--format=%H %gs")}
	}
	was := state()
	env := worktreeEnv(t, t.TempDir(), time.Now())
	dir, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("index123"), repo, nil, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if now := state(); now != was {
		t.Fatalf("checkout changed:\n%q\nwas\n%q", now, was)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "staged.txt")); err != nil || string(data) != "staged only\n" {
		t.Errorf("staged.txt = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "moved.txt")); err != nil || string(data) != "one\n" {
		t.Errorf("moved.txt = %q, %v", data, err)
	}
	for _, gone := range []string{"tracked.txt", "sub/inner/keep.txt"} {
		if _, err := os.Lstat(filepath.Join(dir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still in the worktree: %v", gone, err)
		}
	}
}

// The worktree starts at the commit the changes were recorded against, even
// if the checkout's HEAD moves before the worktree is added.
func TestHandoffWorktreeStartsWhereTheChangesWereRecorded(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	start := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "two\n", 0o644)
	env := worktreeEnv(t, t.TempDir(), time.Now())
	env.RunGit = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		out, err := testGit(ctx, dir, args...)
		if len(args) > 1 && args[0] == "stash" && args[1] == "create" {
			// Another agent commits in the checkout meanwhile.
			if _, commitErr := testGit(ctx, repo, "commit", "-q", "--allow-empty", "-m", "meanwhile"); commitErr != nil {
				t.Error(commitErr)
			}
		}
		return out, err
	}
	dir, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("race1234"), repo, nil, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(mustGit(t, dir, "rev-parse", "HEAD")); got != start {
		t.Fatalf("worktree HEAD = %s, want %s", got, start)
	}
}

// A merge under way would arrive as a plain edit without its second parent,
// so --worktree refuses it before creating anything; so does a repository
// with no commit.
func TestHandoffWorktreeRefusesAnOperationInProgress(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	mustGit(t, repo, "checkout", "-q", "-b", "other")
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "other\n", 0o644)
	mustGit(t, repo, "commit", "-q", "-am", "other")
	mustGit(t, repo, "checkout", "-q", "main")
	writeTestFile(t, filepath.Join(repo, "staged.txt"), "main\n", 0o644)
	mustGit(t, repo, "commit", "-q", "-am", "main")
	mustGit(t, repo, "merge", "-q", "--no-commit", "--no-ff", "other")
	env := worktreeEnv(t, t.TempDir(), time.Now())
	_, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("merge123"), repo, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "a merge is in progress") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Lstat(repo + "-handoff-merge123"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree created during a merge: %v", statErr)
	}

	empty := filepath.Join(resolvedPath(t, t.TempDir()), "empty")
	writeTestFile(t, filepath.Join(empty, "a.txt"), "a\n", 0o644)
	mustGit(t, empty, "init", "-q", "-b", "main")
	if _, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("empty123"), empty, nil, nil, io.Discard); err == nil || !strings.Contains(err.Error(), "needs a commit") {
		t.Fatalf("no commit: err=%v", err)
	}
}

func TestHandoffWorktreeNotesSubmodules(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitmodules"), "", 0o644)
	var stderr bytes.Buffer
	env := worktreeEnv(t, t.TempDir(), time.Now())
	if _, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("submod12"), repo, nil, nil, &stderr); err != nil || !strings.Contains(stderr.String(), "submodules are not checked out") {
		t.Fatalf("err=%v stderr=%q", err, stderr.String())
	}
}

// A destination that cannot be launched fails before a worktree is made.
func TestHandoffWorktreeMissingAgentCreatesNothing(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	initTestRepo(t, f.project)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.RunGit = testGit
	f.env.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("launched without an agent")
		return nil
	}
	_, errOut, code := runHandoff(t, f.env, f.id, "--to", "claude", "--worktree")
	if code == 0 || !strings.Contains(errOut, "could not find") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if list := mustGit(t, f.project, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Fatalf("worktree created for a missing agent:\n%s", list)
	}
}

// check-ref-format --branch accepts "@" (HEAD's shorthand), which is no name
// for a new branch; a leading "-" would read as an option.
func TestHandoffWorktreeRefusesNonsenseBranchNames(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	env := worktreeEnv(t, t.TempDir(), time.Now())
	for _, name := range []string{"@", "-b", "--force"} {
		_, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true, branch: name}, worktreeTarget("nonsense"), repo, nil, nil, io.Discard)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q is not a valid branch name", name)) {
			t.Errorf("--branch %s: err=%v", name, err)
		}
	}
	if _, statErr := os.Lstat(repo + "-handoff-nonsense"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree created for a nonsense branch: %v", statErr)
	}
}

// `git stash create` cannot record an intent-to-add entry, so --worktree
// names the file and creates nothing.
func TestHandoffWorktreeRefusesIntentToAddFiles(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "planned.txt"), "later\n", 0o644)
	mustGit(t, repo, "add", "-N", "planned.txt")
	env := worktreeEnv(t, t.TempDir(), time.Now())
	_, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("intent12"), repo, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "git add -N` (planned.txt)") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Lstat(repo + "-handoff-intent12"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("worktree created with an intent-to-add file: %v", statErr)
	}
	if branches := mustGit(t, repo, "branch", "--list", "handoff/*"); branches != "" {
		t.Fatalf("branch created: %q", branches)
	}
}

// A rebase stopped in a linked worktree is found in that worktree's own git
// directory, and a bisect is allowed with a note.
func TestHandoffWorktreeOperationsInALinkedWorktree(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	linked := repo + "-linked"
	mustGit(t, repo, "worktree", "add", "-q", "-b", "linked", linked)
	writeTestFile(t, filepath.Join(linked, "tracked.txt"), "linked\n", 0o644)
	mustGit(t, linked, "commit", "-q", "-am", "linked")
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "main\n", 0o644)
	mustGit(t, repo, "commit", "-q", "-am", "main")
	if _, err := testGit(t.Context(), linked, "rebase", "main"); err == nil {
		t.Fatal("rebase did not stop on its conflict")
	}
	env := worktreeEnv(t, t.TempDir(), time.Now())
	_, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("rebase12"), linked, nil, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "a rebase is in progress") {
		t.Fatalf("rebase: err=%v", err)
	}
	mustGit(t, linked, "rebase", "--abort")

	mustGit(t, linked, "bisect", "start", "HEAD", "HEAD~1")
	writeTestFile(t, filepath.Join(linked, "staged.txt"), "bisecting\n", 0o644)
	var stderr bytes.Buffer
	dir, err := prepareLaunchDir(env, handoffOptions{to: "claude", worktree: true}, worktreeTarget("bisect12"), linked, nil, nil, &stderr)
	if err != nil || !strings.Contains(stderr.String(), "a bisect is in progress") {
		t.Fatalf("bisect: err=%v stderr=%q", err, stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(dir, "staged.txt")); err != nil || string(data) != "bisecting\n" {
		t.Errorf("staged.txt = %q, %v", data, err)
	}
}

// On a terminal --worktree needs no --to: the agent chosen at the prompt
// starts in the worktree, and choosing to print creates none.
func TestHandoffPromptWithWorktree(t *testing.T) {
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
	want := resolvedPath(t, f.project) + "-handoff-" + handoffShortID(handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: f.id}})
	var pager string
	f.env.RunPager = copyPager(&pager)
	if _, errOut, code := runPicker(t, f.env, "p\n", f.id, "--worktree"); code != 0 || !strings.Contains(errOut, "no worktree was created") {
		t.Fatalf("print: code=%d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("printing created %s: %v", want, err)
	}
	if _, errOut, code := runPicker(t, f.env, "q\n", f.id, "--worktree"); code != 0 || !strings.Contains(errOut, "no worktree was created") {
		t.Fatalf("quit: code=%d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) || got.Dir != "" {
		t.Fatalf("quitting created %s (%v) or launched in %q", want, err, got.Dir)
	}
	if _, errOut, code := runPicker(t, f.env, "\n", f.id, "--worktree"); code != 0 || got.Dir != want {
		t.Fatalf("launch: code=%d dir=%q stderr=%s, want %s", code, got.Dir, errOut, want)
	}
}

// Run by an agent (no terminal), --to --worktree opens the new window in the
// worktree, and an active source is warned about, never asked about.
func TestHandoffNewWindowWithWorktree(t *testing.T) {
	t.Parallel()
	for _, worktree := range []bool{true, false} {
		f := newHandoffFixture(t, false)
		initTestRepo(t, f.project)
		info, err := os.Stat(filepath.Join(f.project, "codex.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		f.env.Now = func() time.Time { return info.ModTime().Add(time.Minute) }
		f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
		f.env.RunGit = testGit
		f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
			t.Error("ran the agent without a terminal")
			return nil
		}
		var spec termlaunch.Spec
		var tmux []string
		f.env.OpenTerminal = openInFakeTmux(&spec, &tmux)
		args := []string{f.id, "--to", "claude"}
		want := resolvedPath(t, f.project)
		if worktree {
			args = append(args, "--worktree")
			want += "-handoff-" + handoffShortID(handoffTarget{bundle: archive.SourceBundle{ArchiveSessionID: f.id}})
		}
		_, errOut, code := runHandoff(t, f.env, args...)
		if code != 0 || strings.Contains(errOut, "[y/N/w]") || !strings.Contains(errOut, "opened claude in a new tmux window") {
			t.Fatalf("worktree=%v: code=%d stderr=%q", worktree, code, errOut)
		}
		if resolvedPath(t, spec.Dir) != want || len(tmux) < 4 || resolvedPath(t, tmux[3]) != want {
			t.Fatalf("worktree=%v: spec.Dir=%q tmux=%q, want %s", worktree, spec.Dir, tmux, want)
		}
		// --worktree already gives the new agent its own checkout.
		if warned := strings.Contains(errOut, "warning: the source session was active"); warned == worktree {
			t.Fatalf("worktree=%v: stderr=%q", worktree, errOut)
		}
	}
}
