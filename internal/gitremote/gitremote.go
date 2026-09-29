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

// ExecRunner runs the git found on PATH, without a shell, in dir. Its
// environment carries none of the caller's GIT_* variables (a stray GIT_DIR
// would answer for another repository), git never prompts, and only the first
// few kilobytes of output are read.
func ExecRunner(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("project directory is gone")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Dir = dir
	cmd.Env = environment(os.Environ())
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	var out limitedBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
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

// limitedBuffer keeps the first maxOutput bytes written and discards the rest.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxOutput - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
