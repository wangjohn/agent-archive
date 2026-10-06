package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestStalePrivateIndexSweepPreservesLiveAndUnsafeDirectories(t *testing.T) {
	t.Parallel()
	root := indexSnapshotRoot(t.TempDir())
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale := now.Add(-2 * time.Hour)
	dirs := map[string]string{}
	for _, name := range []string{"abandoned", "live", "no-lock", "unsafe", "unknown"} {
		prefix := privateIndexPrefix
		if name == "unknown" {
			prefix = "unrelated-"
		}
		dir := filepath.Join(root, prefix+name+"-synthetic")
		err := os.Mkdir(dir, 0700)
		if err != nil {
			t.Fatal(err)
		}
		dirs[name] = dir
		if err := os.WriteFile(filepath.Join(dir, "current.sqlite"), []byte("synthetic index"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	abandoned, err := lockPrivateIndex(dirs["abandoned"])
	if err != nil {
		t.Fatal(err)
	}
	if err := abandoned.Close(); err != nil {
		t.Fatal(err)
	}
	live, err := lockPrivateIndex(dirs["live"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	for _, name := range []string{"abandoned", "live"} {
		path := filepath.Join(dirs[name], privateIndexLock)
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"no-lock", "unsafe", "unknown"} {
		if err := os.Chtimes(dirs[name], stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dirs["unsafe"], 0755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(root, privateIndexPrefix+"symlink")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := sweepPrivateIndexes(t.Context(), root, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"abandoned", "no-lock"} {
		if _, err := os.Stat(dirs[name]); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("abandoned snapshot retained", name, err)
		}
	}
	for _, name := range []string{"live", "unsafe", "unknown"} {
		if _, err := os.Stat(dirs[name]); err != nil {
			t.Fatal("live/unknown directory removed", name, err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("symlink removed", err)
	}
}

func TestStalePrivateIndexSweepResumesBeyondOneDirectoryBatch(t *testing.T) {
	t.Parallel()
	root := indexSnapshotRoot(t.TempDir())
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for n := range 400 {
		dir := filepath.Join(root, privateIndexPrefix+strconv.Itoa(n))
		err := os.Mkdir(dir, 0700)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	for pass := range 24 {
		if err := sweepPrivateIndexes(t.Context(), root, now); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		remaining := 0
		for _, entry := range entries {
			if entry.IsDir() {
				remaining++
			}
		}
		if remaining == 0 {
			return
		}
		if pass == 23 {
			t.Fatal("bounded sweep did not converge", remaining)
		}
	}
}
