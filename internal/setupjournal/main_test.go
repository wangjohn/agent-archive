package setupjournal

import (
	"os"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
	"github.com/wangjohn/agent-archive/internal/testutil/isolation"
)

// testTempPrefix names the folder under /tmp that holds one test run's
// temporary files.
const testTempPrefix = "agent-archive-setupjournal-test-"

// TestMain runs every test in a process that cannot reach this Mac's real
// home, app configuration, or temporary folder. launchd is reached only
// through the Launchd a caller passes, and this package imports nothing that
// runs launchctl (TestSetupJournalImportBoundary), so a test that forgets its
// fakeLaunchd cannot compile rather than reach the real one.
func TestMain(m *testing.M) {
	restore := isolation.Process(testTempPrefix)
	code := m.Run()
	restore()
	os.Exit(code)
}

// TestIsolationFailsClosed pins TestMain's isolation.
func TestIsolationFailsClosed(t *testing.T) {
	t.Parallel()
	isolation.Check(t, testTempPrefix)
}
