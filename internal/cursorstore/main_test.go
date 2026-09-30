package cursorstore

import (
	"os"
	"testing"
)

// TestMain unsets XDG_CONFIG_HOME. On Linux StateDatabase honors it, so with
// it set in the developer's shell a test that asks for the database under a
// temporary home would be sent to their real Cursor data folder instead.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
