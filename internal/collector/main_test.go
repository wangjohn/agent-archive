package collector

import (
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// TestMain keeps every sweep of leftover Cursor database copies (each
// collector pass runs one) in a temporary folder of the test run's own,
// never the user's real snapshot folder.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "collector-snapshots-")
	if err != nil {
		panic(err)
	}
	cursorstore.SnapshotTempDirForTesting = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
