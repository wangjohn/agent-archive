package gitremote

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// objectName is a full git object name: SHA-1 (40 hex digits) or SHA-256
// (64), as `git rev-parse` prints it.
var objectName = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Head returns the full object name of the commit checked out in the
// repository holding dir, or "" when there is none (not a repository, a
// branch with no commits yet) or git cannot be asked within Timeout. It runs
// `git -C dir rev-parse --verify --quiet HEAD^{commit}`, which answers for a
// subdirectory, a linked worktree (its own HEAD), and a detached HEAD alike.
func Head(ctx context.Context, dir string, run Runner) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(ctx, dir, "-C", dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil || ctx.Err() != nil {
		return ""
	}
	sha := strings.TrimSpace(string(out))
	if !objectName.MatchString(sha) {
		return ""
	}
	return sha
}

// Dirty reports whether the working tree of the repository holding dir
// differs from its HEAD: a staged or unstaged change to a tracked file, or an
// untracked file that is not ignored. It is nil when git cannot tell within
// Timeout. It runs `git status --porcelain` and reads only whether it printed
// anything; what it printed (file names) is never kept. A submodule counts
// when its checked-out commit differs, not for changes inside it, which git
// would have to walk the submodule to find.
func Dirty(ctx context.Context, dir string, run Runner) *bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	if run == nil {
		run = ExecRunner
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := run(ctx, dir, "-C", dir, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=dirty")
	if ctx.Err() != nil {
		return nil
	}
	// Output past the cap is still output: the tree is dirty.
	if err != nil && (!errors.Is(err, ErrOutputLimit) || len(out) == 0) {
		return nil
	}
	dirty := len(strings.TrimSpace(string(out))) > 0
	return &dirty
}

// HeadState is Head and, when withDirty is set, Dirty, asked at the same time
// so the two together take no longer than one. dirty is nil when not asked
// for, when git could not tell, and whenever sha is "" (a dirty flag means
// nothing without the commit it is relative to).
func HeadState(dir string, withDirty bool, run Runner) (sha string, dirty *bool) {
	ctx := context.Background()
	var wg sync.WaitGroup
	if withDirty {
		wg.Go(func() { dirty = Dirty(ctx, dir, run) })
	}
	sha = Head(ctx, dir, run)
	wg.Wait()
	if sha == "" {
		return "", nil
	}
	return sha, dirty
}
