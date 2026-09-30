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

// TestUserTempDir: macOS uses the per-user temporary directory the system
// reports, whatever $TMPDIR says, so a collector started by launchd without
// TMPDIR, a hook run with it, and a shell with a custom one share one
// snapshot root; $TMPDIR only when the system can't say, and elsewhere.
func TestUserTempDir(t *testing.T) {
	env := func(tmp string) func(string) string {
		return func(key string) string {
			if key == "TMPDIR" {
				return tmp
			}
			return ""
		}
	}
	perUser := func() string { return "/var/folders/xy/abc/T" }
	for _, tc := range []struct {
		name   string
		tmpdir string
		goos   string
		darwin func() string
		want   string
	}{
		{"custom TMPDIR", "/private/tmp/mine", "darwin", perUser, "/var/folders/xy/abc/T"},
		{"launchd without TMPDIR", "", "darwin", perUser, "/var/folders/xy/abc/T"},
		{"getconf failed", "/private/tmp/mine", "darwin", func() string { return "" }, "/private/tmp/mine"},
		{"getconf failed, no TMPDIR", "", "darwin", func() string { return "" }, fallbackTemp()},
		{"not macOS", "/tmp/linux", "linux", perUser, "/tmp/linux"},
		{"Linux without TMPDIR", "", "linux", perUser, fallbackTemp()},
		// A relative $TMPDIR is ignored as if unset: the snapshot root, a
		// copy of every chat, must not depend on the working directory.
		{"Linux relative TMPDIR", "tmp", "linux", perUser, fallbackTemp()},
		{"Linux dot-relative TMPDIR", "./tmp", "linux", perUser, fallbackTemp()},
		{"Linux tilde TMPDIR", "~/tmp", "linux", perUser, fallbackTemp()},
		{"macOS relative TMPDIR, getconf failed", "tmp", "darwin", func() string { return "" }, fallbackTemp()},
		{"macOS relative TMPDIR, getconf answers", "tmp", "darwin", perUser, "/var/folders/xy/abc/T"},
		{"macOS absolute TMPDIR, getconf failed", "/private/tmp/mine", "darwin", func() string { return "" }, "/private/tmp/mine"},
		{"Linux absolute TMPDIR", "/var/tmp/mine", "linux", perUser, "/var/tmp/mine"},
	} {
		if got := userTempDir(env(tc.tmpdir), tc.goos, tc.darwin); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if runtime.GOOS == "darwin" {
		if dir := darwinUserTempDir(); !filepath.IsAbs(dir) {
			t.Fatalf("DARWIN_USER_TEMP_DIR = %q", dir)
		}
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

// With the process's own $TMPDIR relative, os.TempDir would return it; the
// snapshot root still falls back to /tmp rather than a working-directory
// path.
func TestUserTempDirIgnoresARelativeProcessTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", "relative/tmp")
	if runtime.GOOS == "windows" {
		t.Skip("no /tmp")
	}
	for _, goos := range []string{"linux", "darwin"} {
		got := userTempDir(func(string) string { return "relative/tmp" }, goos, func() string { return "" })
		if got != "/tmp" {
			t.Errorf("%s: %q, want /tmp", goos, got)
		}
	}
}

// fallbackTemp is what userTempDir answers when neither the system nor an
// absolute $TMPDIR says: os.TempDir when that is absolute, else /tmp.
func fallbackTemp() string {
	if dir := os.TempDir(); filepath.IsAbs(dir) {
		return dir
	}
	return "/tmp"
}
