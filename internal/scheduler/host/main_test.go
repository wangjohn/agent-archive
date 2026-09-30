package host

import (
	"os"
	"testing"
)

// TestMain empties PATH for the whole package, so a test that forgets to
// stub launchctl finds no launchctl at all instead of the real one. Tests
// that run a stand-in set their own PATH with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-archive-host-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("PATH", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
