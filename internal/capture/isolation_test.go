package capture

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/isolation"
)

// testTempPrefix names the folder under /tmp that holds one test run's
// temporary files.
const testTempPrefix = "agent-archive-capture-test-"

// TestIsolationFailsClosed pins TestMain's isolation. capture takes its data
// directory as an argument and cannot reach launchctl, the Keychain, or the
// network at all (its imports forbid them; see imports_test.go), so what is
// isolated is the process environment a test could fall back to by mistake:
// the process's own home and temporary folder are the run's, and no variable
// that moves a data or app directory leaks in.
func TestIsolationFailsClosed(t *testing.T) {
	t.Parallel()
	isolation.Check(t, testTempPrefix)
}
