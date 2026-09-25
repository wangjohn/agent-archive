package isolation

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/local"
)

const testPrefix = "agent-archive-isolation-test-"

func TestMain(m *testing.M) {
	restore := Process(testPrefix)
	code := m.Run()
	restore()
	os.Exit(code)
}

func TestProcessIsolates(t *testing.T) {
	t.Parallel()
	Check(t, testPrefix)
}

// The go command run from an isolated test (importgraph's go list) keeps the
// developer's module and build caches. Under the temporary home it would
// start with empty ones and download every module: slow, a network access
// from a test, and a failure offline.
func TestGoCommandKeepsItsCaches(t *testing.T) {
	t.Parallel()
	out, err := exec.CommandContext(t.Context(), "go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		t.Skipf("no go command: %v", err)
	}
	home := os.Getenv("HOME")
	for _, dir := range strings.Fields(string(out)) {
		if local.PathWithin(dir, home) {
			t.Errorf("the go command keeps %s in the test's temporary home %s", dir, home)
		}
	}
}
