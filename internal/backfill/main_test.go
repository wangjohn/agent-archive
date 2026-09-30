package backfill

import (
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// TestMain keeps every copy of a test's Cursor database, and every sweep of
// leftover copies, in a temporary folder of the test run's own, never the
// user's real snapshot folder.
//
// It also unsets XDG_CONFIG_HOME: on Linux the Cursor database path honors
// it, so with it set in the developer's shell a test that asks for the
// database under a temporary home would be sent to their real Cursor data.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "backfill-snapshots-")
	if err != nil {
		panic(err)
	}
	cursorstore.SnapshotTempDirForTesting = dir
	code := m.Run()
	// A folder left behind holds only this run's copies; the run's result
	// stands either way.
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
