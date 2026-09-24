package cli

import (
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// TestMain keeps every copy of a test's Cursor database, and every sweep of
// leftover copies, in a temporary folder of the test run's own, never the
// user's real snapshot folder, which a live collector may be using.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cli-snapshots-")
	if err != nil {
		panic(err)
	}
	cursorstore.SnapshotTempDirForTesting = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
