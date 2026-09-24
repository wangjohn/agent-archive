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
	for _, d := range []string{unlocked, legacy} {
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

// TestUserTempDir: $TMPDIR wins; without it macOS uses the per-user
// temporary directory the system reports, not the shared /tmp, so a
// collector started by launchd without TMPDIR and a hook run with it share
// one snapshot root.
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
		name, tmpdir, goos string
		darwin             func() string
		want               string
	}{
		{"TMPDIR set", "/private/tmp/mine", "darwin", perUser, "/private/tmp/mine"},
		{"launchd without TMPDIR", "", "darwin", perUser, "/var/folders/xy/abc/T"},
		{"getconf failed", "", "darwin", func() string { return "" }, os.TempDir()},
		{"not macOS", "", "linux", perUser, os.TempDir()},
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
