package capture

import (
	"os"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// TestMain runs every test in a process that cannot reach this Mac's real
// home, app configuration, or temporary folder (see isolation_test.go).
func TestMain(m *testing.M) {
	restore := isolateProcessForTesting()
	code := m.Run()
	restore()
	os.Exit(code)
}
