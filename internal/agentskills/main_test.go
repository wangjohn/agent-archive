package agentskills

import (
	"os"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
	"github.com/wangjohn/agent-archive/internal/testutil/isolation"
)

// testTempPrefix names the folder under /tmp that holds one test run's
// temporary files.
const testTempPrefix = "agent-archive-agentskills-test-"

// TestMain runs every test in a process that cannot reach this machine's real
// home, where the skills live.
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
