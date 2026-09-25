// Package isolation keeps a package's tests away from this Mac's real home,
// app configuration, and temporary folder. A package whose production code
// cannot reach launchctl or the Keychain at all (its imports forbid them)
// calls Process from its TestMain and Check from a test; internal/cli, which
// can, has its own stricter TestMain on top of the same idea.
//
// It is test code only; depguard keeps it out of production packages.
package isolation

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Variables names the environment variables that move a data, app, or AWS
// configuration directory. Process unsets them all.
var Variables = []string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE"}

// Process isolates the test process, before any test runs:
//
//   - $HOME is a fresh temporary directory, and every variable in Variables
//     is unset, so nothing that resolves a default location lands in the
//     developer's own ~/.claude, ~/.cursor, ~/.local/share/agent-archive, or
//     ~/.aws.
//
//   - $TMPDIR, and so every t.TempDir, is a fresh folder under /tmp named
//     with prefix, rather than macOS's per-user temporary folder.
//
// It panics if it cannot, and returns a function that removes the folders.
func Process(prefix string) (restore func()) {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	parent := ""
	if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
		parent = "/tmp"
	}
	tmp, err := os.MkdirTemp(parent, prefix)
	must(err)
	must(os.Setenv("TMPDIR", tmp))
	home, err := os.MkdirTemp("", prefix+"home-")
	must(err)
	must(os.Setenv("HOME", home))
	for _, name := range Variables {
		must(os.Unsetenv(name))
	}
	return func() { _ = os.RemoveAll(home); _ = os.RemoveAll(tmp) }
}

// Check fails the test unless Process(prefix) isolated this process: the
// home is not the account's real one, no variable in Variables is set, and
// t.TempDir is inside the run's own folder.
func Check(t *testing.T, prefix string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if account, err := user.Current(); err == nil && filepath.Clean(account.HomeDir) == filepath.Clean(home) {
		t.Fatalf("tests run with the account's real home %s", home)
	}
	for _, name := range Variables {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s=%s leaks into the tests", name, value)
		}
	}
	if dir := t.TempDir(); !strings.Contains(dir, prefix) {
		t.Errorf("t.TempDir() = %s, outside this run's temporary folder", dir)
	}
}
