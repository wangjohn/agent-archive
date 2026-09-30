// Package gitremote finds a project's git origin remote and turns it into an
// archive.RepoKey, best effort. It is the only place the program runs git.
//
// Every failure (git not installed, a directory that is not a repository, no
// origin, a slow disk) is an empty result, never an error: a repository key
// is an optimization for matching sessions across machines, and no caller may
// fail, or wait long, for it. The hook runtime imports nothing that runs a
// program, so it is handed a resolver by the command line instead (see
// internal/cli's `_hook`).
package gitremote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Timeout bounds one git invocation. A hook has about two seconds in all.
const Timeout = 500 * time.Millisecond

// maxOutput is the most git output read; a remote URL is far shorter.
const maxOutput = 4096

// Runner runs git with args in dir and returns its standard output. It must
// stop when ctx ends. Tests substitute one; ExecRunner is the real one.
type Runner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// OriginURL returns the URL of root's origin remote, as git reports it, or ""
// when there is none or git cannot be asked within Timeout. run is nil for
// ExecRunner. It runs `git -C root config --get remote.origin.url`, which
// also answers for a subdirectory of a repository and for a linked worktree.
//
// The URL can carry credentials (https://user:token@host/...), so it is for
// archive.RepoKey and must not be stored or logged.
func OriginURL(ctx context.Context, root string, run Runner) string {
	if root == "" || !filepath.IsAbs(root) {
		return ""
	}
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(ctx, root, "-C", root, "config", "--get", "remote.origin.url")
	if err != nil || ctx.Err() != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// RepoKey is archive.RepoKey of root's origin remote, or "" (see OriginURL).
func RepoKey(ctx context.Context, root string, run Runner) string {
	return archive.RepoKey(OriginURL(ctx, root, run))
}

// Resolver derives repository keys and remembers each project root's answer,
// so a sweep over many sessions of one project asks git once. It is safe for
// concurrent use. The zero value runs the real git.
type Resolver struct {
	// Run replaces ExecRunner; tests set it.
	Run Runner

	mu    sync.Mutex
	cache map[string]string
}

// Key returns RepoKey(root), from the cache after the first call for root.
// A failure is cached too, so an unreachable git costs one timeout per
// project, not one per session.
func (r *Resolver) Key(root string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key, ok := r.cache[root]; ok {
		return key
	}
	key := RepoKey(context.Background(), root, r.Run)
	if r.cache == nil {
		r.cache = map[string]string{}
	}
	r.cache[root] = key
	return key
}

// ExecRunner runs the git found on PATH, without a shell. The directory to
// ask about is in args (-C), not the process's working directory, and is not
// checked first: a stat of a hung mount would block with nothing to stop it,
// and git's own failure on a missing directory is the same answer. The
// environment carries none of the caller's GIT_* variables (a stray GIT_DIR
// would answer for another repository), git never prompts, and output past a
// few kilobytes is an error, not a truncated URL.
func ExecRunner(ctx context.Context, _ string, args ...string) ([]byte, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	if git == macGitStub && !stubUsable() {
		return nil, errors.New("git is not installed (the macOS stub needs the developer tools)")
	}
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Env = environment(os.Environ())
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	var out limitedBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if out.over {
		return nil, errors.New("git printed more than a remote URL")
	}
	return out.buf.Bytes(), nil
}

// macOS ships /usr/bin/git as a stub that, without the developer tools, opens
// a graphical "install developer tools" prompt instead of running. A hook or
// the LaunchAgent must never do that.
const macGitStub = "/usr/bin/git"

// developerGits are where the developer tools keep the real git.
var developerGits = []string{
	"/Library/Developer/CommandLineTools/usr/bin/git",
	"/Applications/Xcode.app/Contents/Developer/usr/bin/git",
}

var (
	gitUsableOnce   sync.Once
	gitUsableResult bool
)

// usableGit reports whether git, as LookPath found it, can be run without the
// macOS stub's prompt: anything but the stub is, and the stub is when the
// developer tools provide a git (a DEVELOPER_DIR pointing elsewhere is not
// consulted: refusing is the safe error). exists is a file check, a parameter
// for tests.
func usableGit(path, goos string, exists func(string) bool) bool {
	if goos != "darwin" || path != macGitStub {
		return true
	}
	return slices.ContainsFunc(developerGits, exists)
}

// stubUsable is usableGit for the real stub, answered once per process.
func stubUsable() bool {
	gitUsableOnce.Do(func() {
		gitUsableResult = usableGit(macGitStub, runtime.GOOS, fileExists)
	})
	return gitUsableResult
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// environment is env without any GIT_* variable, plus the settings that keep
// git from prompting, taking optional locks, or localizing its output.
func environment(env []string) []string {
	kept := make([]string, 0, len(env)+3)
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			kept = append(kept, entry)
		}
	}
	return append(kept, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
}

// limitedBuffer keeps the first maxOutput bytes written, discards the rest,
// and records that it did. The buffer is a field, not embedded: embedding
// would promote bytes.Buffer's ReadFrom, which io.Copy prefers, and skip this
// Write's cap.
type limitedBuffer struct {
	buf  bytes.Buffer
	over bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	room := maxOutput - b.buf.Len()
	if len(p) > room {
		b.over = true
	}
	if room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
