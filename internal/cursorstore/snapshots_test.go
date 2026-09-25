package cursorstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		{"getconf failed, no TMPDIR", "", "darwin", func() string { return "" }, os.TempDir()},
		{"not macOS", "/tmp/linux", "linux", perUser, "/tmp/linux"},
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
