package cursorstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
)

// TestSweepSkipsASnapshotInUse: a Reader's copy is never swept while the
// Reader holds it, however old its directory, by this process or another
// (the lock is an flock, which a second open file conflicts with); once the
// Reader closes, nothing is left.
func TestSweepSkipsASnapshotInUse(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	startWriter(t, path).put(chatRows())
	r := NewReader(path)
	if _, _, err := r.ReadComposer(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot directories %v, %v", entries, err)
	}
	dir := filepath.Join(root, entries[0].Name())
	old := time.Now().Add(-2 * staleSnapshotAge)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	// A stale directory whose lock nobody holds, and one without a lock
	// file, are removed.
	unlocked := filepath.Join(root, snapshotPrefix+"unlocked")
	legacy := filepath.Join(root, snapshotPrefix+"legacy")
	for _, d := range []string{unlocked, legacy} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(unlocked, snapshotLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{unlocked, legacy, filepath.Join(unlocked, snapshotLockName)} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	RemoveStaleSnapshots()
	if _, err := os.Stat(filepath.Join(dir, "state.vscdb")); err != nil {
		t.Fatalf("a copy in use was swept: %v", err)
	}
	for _, d := range []string{unlocked, legacy} {
		if _, err := os.Stat(d); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s kept: %v", filepath.Base(d), err)
		}
	}
	// Still readable after the sweep.
	if _, _, err := r.ReadComposer(context.Background(), "c"); err != nil || r.Snapshots() != 1 {
		t.Fatalf("after the sweep: %v, %d snapshots", err, r.Snapshots())
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, root)
}

// TestSweepRemovesAnAbandonedSnapshotPromptly: a process killed mid-read
// (Ctrl-C during a backfill plan) leaves a directory whose lock nobody
// holds. It is removed as soon as its lock file is a minute old, not after
// the hour a directory without a lock file waits; one whose lock file was
// just created may be a read starting, and is kept.
func TestSweepRemovesAnAbandonedSnapshotPromptly(t *testing.T) {
	root := useTempSnapshots(t)
	abandoned := filepath.Join(root, snapshotPrefix+"abandoned")
	starting := filepath.Join(root, snapshotPrefix+"starting")
	for _, d := range []string{abandoned, starting} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, snapshotLockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "state.vscdb"), []byte("copy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	twoMinutes := time.Now().Add(-2 * abandonedSnapshotAge)
	if err := os.Chtimes(filepath.Join(abandoned, snapshotLockName), twoMinutes, twoMinutes); err != nil {
		t.Fatal(err)
	}
	RemoveStaleSnapshots()
	if _, err := os.Stat(abandoned); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("abandoned snapshot kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(starting, "state.vscdb")); err != nil {
		t.Fatalf("a snapshot just starting was swept: %v", err)
	}
}

// TestSnapshotRootIsTheSystemsOwn: the snapshot root is
// platform.Locations.SnapshotRoot wired to this package's getconf, this
// process's environment and the account's home (the choice itself, on both
// systems, is tested in internal/platform). On macOS it is under the per-user
// temporary directory the system reports, whatever $TMPDIR says, so a
// collector started by launchd without TMPDIR and a shell with a custom one
// share one root. On Linux it is under the XDG cache home: $XDG_CACHE_HOME,
// else .cache in the account's home, whatever $HOME and $TMPDIR say.
func TestSnapshotRootIsTheSystemsOwn(t *testing.T) {
	mine := t.TempDir()
	t.Setenv("TMPDIR", mine)
	switch runtime.GOOS {
	case "darwin":
		dir := darwinUserTempDir()
		if !filepath.IsAbs(dir) {
			t.Fatalf("DARWIN_USER_TEMP_DIR = %q", dir)
		}
		if got, want := snapshotRootPath(), filepath.Join(dir, platform.SnapshotDirName(os.Getuid())); got != want {
			t.Errorf("on macOS the root is %q, want %q", got, want)
		}
		if got := snapshotCacheDir(); got != "" {
			t.Errorf("on macOS the cache directory is %q, want none", got)
		}
	case "linux":
		cache := t.TempDir()
		t.Setenv("XDG_CACHE_HOME", cache)
		if got, want := snapshotRootPath(), filepath.Join(cache, "agent-archive", "cursor-snapshots"); got != want {
			t.Errorf("on Linux the root is %q, want %q", got, want)
		}
		if got, want := snapshotCacheDir(), filepath.Join(cache, "agent-archive"); got != want {
			t.Errorf("on Linux the cache directory is %q, want %q", got, want)
		}
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("HOME", t.TempDir())
		if home := accountHome(); filepath.IsAbs(home) {
			if got, want := snapshotRootPath(), filepath.Join(home, ".cache", "agent-archive", "cursor-snapshots"); got != want {
				t.Errorf("on Linux without XDG_CACHE_HOME the root is %q, want %q", got, want)
			}
		}
	}
	// The real system's own process environment and account are what is read.
	if got, want := snapshotRootPath(), currentSnapshotLocations().SnapshotRoot(); got != want {
		t.Errorf("snapshotRootPath = %q, want %q", got, want)
	}
}

// A system the program does not know has no snapshot directory: no path, and
// SnapshotRoot fails closed instead of using a shared temporary directory.
func TestUnknownSystemHasNoSnapshotRoot(t *testing.T) {
	t.Parallel()
	if got := snapshotLocations(platform.Unknown, func(string) string { return "/tmp/mine" }, "/home/ada").SnapshotRoot(); got != "" {
		t.Errorf("the snapshot root on an unknown system is %q, want none", got)
	}
	if root, err := preparedSnapshotRoot("", ""); !errors.Is(err, errSnapshotUnsupportedSystem) || root != "" {
		t.Errorf("preparedSnapshotRoot(\"\") = %q, %v; want errSnapshotUnsupportedSystem", root, err)
	}
}

// On Linux with neither XDG_CACHE_HOME nor an account home there is no root,
// and SnapshotRoot fails closed rather than use a temporary directory.
func TestLinuxWithoutACacheHomeHasNoSnapshotRoot(t *testing.T) {
	t.Parallel()
	got := snapshotLocations(platform.Linux, func(name string) string { return map[string]string{"TMPDIR": "/tmp/mine"}[name] }, "")
	if got.SnapshotRoot() != "" || got.SnapshotCacheDir() != "" {
		t.Errorf("root %q, cache directory %q, want none", got.SnapshotRoot(), got.SnapshotCacheDir())
	}
}

// TestSnapshotRootErrorSaysWhatToDo: the error for a snapshot directory
// that is not private names it and says to remove it.
func TestSnapshotRootErrorSaysWhatToDo(t *testing.T) {
	root := useTempSnapshots(t)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := SnapshotRoot()
	if !errors.Is(err, errSnapshotRootNotPrivate) || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "remove it") {
		t.Fatalf("err %v", err)
	}
}

// Regression: 2026-09 review B-24. A process about to exit on a second
// Ctrl-C, SIGTERM, or SIGHUP never closes its Readers; RemoveOwnSnapshots
// removes the copies they hold, locked or not, and leaves other processes'
// alone. A Reader closed normally is no longer tracked.
func TestRemoveOwnSnapshotsRemovesThisProcessCopies(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	startWriter(t, path).put(chatRows())
	closed := NewReader(path)
	if _, _, err := closed.ReadComposer(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	leaked := NewReader(path)
	if _, _, err := leaked.ReadComposer(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, snapshotPrefix+"another-process")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 2 {
		t.Fatalf("snapshot directories %v, %v", entries, err)
	}
	RemoveOwnSnapshots()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(other) {
		t.Fatalf("after: %v, %v", entries, err)
	}
	// Closing the Reader afterwards is harmless.
	if err := leaked.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotsArePrivateUnderAnyUmask: a copy of every Cursor chat goes in
// a directory others can neither list nor open, whatever the process's
// umask. It matters most on Linux, where the per-user temporary directory
// is the shared, world-listable /tmp and a container or service may run
// with umask 0. The root and the snapshot directory are 0700, the copy and
// its lock file 0600 (a umask only removes bits).
func TestSnapshotsArePrivateUnderAnyUmask(t *testing.T) {
	for _, umask := range []int{0o000, 0o022, 0o077} {
		t.Run(fmt.Sprintf("umask %03o", umask), func(t *testing.T) {
			root := useTempSnapshots(t)
			path := StateDatabase(t.TempDir())
			startWriter(t, path).put(chatRows())
			old := syscall.Umask(umask)
			t.Cleanup(func() { syscall.Umask(old) })
			checked := false
			hooks := readerHooks{afterSnapshot: func(copyPath string) {
				checked = true
				dir := filepath.Dir(copyPath)
				for p, want := range map[string]fs.FileMode{
					root:                                 0o700,
					dir:                                  0o700,
					copyPath:                             0o600,
					filepath.Join(dir, snapshotLockName): 0o600,
				} {
					info, err := os.Stat(p)
					if err != nil {
						t.Errorf("%s: %v", p, err)
						continue
					}
					if info.Mode().Perm() != want {
						t.Errorf("%s mode %v, want %v", p, info.Mode().Perm(), want)
					}
				}
			}}
			if _, _, err := readComposerWith(context.Background(), path, "c", hooks); err != nil {
				t.Fatal(err)
			}
			if !checked {
				t.Fatal("no snapshot was taken, so no mode was checked")
			}
		})
	}
}

// TestSnapshotRootIsRejectedWhenNotPrivate: a root others could list or
// open, such as one made under a shared /tmp with a permissive umask by
// something else, is refused rather than used.
func TestSnapshotRootIsRejectedWhenNotPrivate(t *testing.T) {
	for _, mode := range []fs.FileMode{0o755, 0o750, 0o701, 0o777} {
		root := useTempSnapshots(t)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := SnapshotRoot(); !errors.Is(err, errSnapshotRootNotPrivate) {
			t.Errorf("mode %v: err %v, want errSnapshotRootNotPrivate", mode, err)
		}
	}
}

// The Linux snapshot root is made with its parents: the cache home (0700, as
// the XDG specification says, when it was missing) and agent-archive's folder
// in it, which gets the CACHEDIR.TAG the Cache Directory Tagging
// Specification describes, so a backup tool that honors it skips the copies.
func TestLinuxSnapshotRootIsMadeUnderTheCacheDirectoryWithATag(t *testing.T) {
	t.Parallel()
	cache := filepath.Join(t.TempDir(), "new-cache-home")
	loc := snapshotLocations(platform.Linux, func(name string) string { return map[string]string{"XDG_CACHE_HOME": cache}[name] }, "/home/ignored")
	root, err := preparedSnapshotRoot(loc.SnapshotRoot(), loc.SnapshotCacheDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cache, "agent-archive", "cursor-snapshots"); root != want {
		t.Fatalf("root %q, want %q", root, want)
	}
	for _, dir := range []string{cache, filepath.Join(cache, "agent-archive"), root} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v; want a directory with mode 0700", dir, info, err)
		}
	}
	tag, err := os.ReadFile(filepath.Join(cache, "agent-archive", "CACHEDIR.TAG"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(tag), "Signature: 8a477f597d28d172789f06886806bc55\n") {
		t.Errorf("CACHEDIR.TAG does not start with the specification's signature: %q", tag)
	}
	// The tag is at agent-archive's folder, not inside the root it covers.
	if _, err := os.Lstat(filepath.Join(root, "CACHEDIR.TAG")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a tag inside the snapshot root: %v", err)
	}
}

// Making the root again changes nothing, and leaves a tag the user wrote (or
// edited) alone.
func TestCacheDirectoryTagIsWrittenOnceAndLeftAlone(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "agent-archive")
	root := filepath.Join(dir, "cursor-snapshots")
	tagPath := filepath.Join(dir, "CACHEDIR.TAG")
	if _, err := preparedSnapshotRoot(root, dir); err != nil {
		t.Fatal(err)
	}
	const mine = "Signature: 8a477f597d28d172789f06886806bc55\n# mine\n"
	if err := os.WriteFile(tagPath, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := preparedSnapshotRoot(root, dir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(tagPath); err != nil || string(got) != mine {
		t.Errorf("tag %q, %v; want the user's own left alone", got, err)
	}
}

// A tag that cannot be written does not stop a read: the copy is removed as
// soon as it is used, so a leftover backed up is an inconvenience.
func TestSnapshotRootWorksWhenTheTagCannotBeWritten(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "agent-archive")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the file would go: O_EXCL refuses, as it does a link.
	if err := os.Mkdir(filepath.Join(dir, "CACHEDIR.TAG"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := preparedSnapshotRoot(filepath.Join(dir, "cursor-snapshots"), dir); err != nil {
		t.Fatalf("a tag that was already there stopped the snapshot root: %v", err)
	}
}

// agent-archive's folder in the cache home must be a real directory of the
// user's that no one else can write to: another account that could write to
// it could replace the snapshot directory inside it after it was checked.
func TestCacheDirectoryMustBePrivate(t *testing.T) {
	t.Parallel()
	for _, mode := range []fs.FileMode{0o770, 0o707, 0o777, 0o720} {
		dir := filepath.Join(t.TempDir(), "agent-archive")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_, err := preparedSnapshotRoot(filepath.Join(dir, "cursor-snapshots"), dir)
		if !errors.Is(err, errCacheDirNotPrivate) || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "remove it") {
			t.Errorf("mode %v: err %v, want errCacheDirNotPrivate naming the directory", mode, err)
		}
		if _, statErr := os.Lstat(filepath.Join(dir, "cursor-snapshots")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("mode %v: a snapshot root was made in a directory others can write to", mode)
		}
	}
	// A mode that others can read but not write is fine: nothing private is
	// in this directory, only the private root under it.
	dir := filepath.Join(t.TempDir(), "agent-archive")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := preparedSnapshotRoot(filepath.Join(dir, "cursor-snapshots"), dir); err != nil {
		t.Errorf("mode 0755: %v", err)
	}
}

// A link where the cache directory should be is refused, whatever it points
// at.
func TestCacheDirectoryIsNotFollowedThroughALink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "agent-archive")
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := preparedSnapshotRoot(filepath.Join(dir, "cursor-snapshots"), dir); !errors.Is(err, errCacheDirNotPrivate) {
		t.Errorf("err %v, want errCacheDirNotPrivate", err)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, "cursor-snapshots")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a snapshot root was made through the link: %v", err)
	}
}

// A cache home that was already there keeps its mode (0755 and 0775 are the
// user's to choose, and are not tightened), but one that every account can
// write to without the sticky bit is refused before anything is made in it:
// anyone could rename agent-archive's folder away and put their own in its
// place. A sticky one (like /tmp) is fine: only its owner can rename what is
// in it.
func TestExistingCacheHomeKeepsItsModeUnlessEveryoneCanWriteToIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode fs.FileMode
		ok   bool
	}{
		{0o755, true},
		{0o775, true},
		{0o700, true},
		{0o777 | fs.ModeSticky, true},
		{0o777, false},
		{0o703, false},
	} {
		cache := filepath.Join(t.TempDir(), "cache")
		if err := os.Mkdir(cache, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cache, tc.mode); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(cache, "agent-archive")
		_, err := preparedSnapshotRoot(filepath.Join(dir, "cursor-snapshots"), dir)
		if tc.ok && err != nil {
			t.Errorf("cache home %v: %v", tc.mode, err)
		}
		if !tc.ok {
			if !errors.Is(err, errCacheDirNotPrivate) || !strings.Contains(err.Error(), cache) || !strings.Contains(err.Error(), "XDG_CACHE_HOME") {
				t.Errorf("cache home %v: err %v, want errCacheDirNotPrivate naming it", tc.mode, err)
			}
			if _, statErr := os.Lstat(dir); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("cache home %v: agent-archive's folder was made in it", tc.mode)
			}
		}
		if info, err := os.Stat(cache); err != nil || info.Mode()&(fs.ModePerm|fs.ModeSticky) != tc.mode {
			t.Errorf("cache home %v: now %v, %v; want its mode left alone", tc.mode, info.Mode(), err)
		}
	}
}
