package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A copy of Cursor's database that a killed backfill left behind is swept
// by the next collector pass, even one with no Cursor database chat to
// read: before, the collector swept only when it read Cursor itself, so on
// a Mac with none registered the copy of every chat stayed in the temporary
// folder until the next backfill.
func TestEveryPassSweepsStaleCursorSnapshots(t *testing.T) {
	previous := cursorstore.SnapshotTempDirForTesting
	cursorstore.SnapshotTempDirForTesting = t.TempDir()
	t.Cleanup(func() { cursorstore.SnapshotTempDirForTesting = previous })
	root := filepath.Join(cursorstore.SnapshotTempDirForTesting, fmt.Sprintf("agent-archive-cursor-%d", os.Getuid()))
	stale := filepath.Join(root, "cursor-snapshot-left-by-a-killed-backfill")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "state.vscdb"), []byte("every chat"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	local := newTestStore(t)
	if _, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "machine"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the pass left the stale snapshot: %v", err)
	}
}
