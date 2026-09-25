package capture

import (
	"os"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
	"github.com/wangjohn/agent-archive/internal/testutil/isolation"
)

// TestMain runs every test in a process that cannot reach this Mac's real
// home, app configuration, or temporary folder (TestIsolationFailsClosed).
func TestMain(m *testing.M) {
	restore := isolation.Process(testTempPrefix)
	code := m.Run()
	restore()
	os.Exit(code)
}
