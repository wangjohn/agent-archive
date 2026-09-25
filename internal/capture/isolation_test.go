package capture

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// testTempPrefix names the folder under /tmp that holds one test run's
// temporary files.
const testTempPrefix = "agent-archive-capture-test-"

// isolateProcessForTesting makes the package's tests fail closed, as
// internal/cli's do. capture takes its data directory as an argument and
// cannot reach launchctl, the Keychain, or the network at all (its imports
// forbid them; see imports_test.go), so what is left to isolate is the
// process environment a test could fall back to by mistake:
//
//   - $HOME is a fresh temporary directory, and AGENT_ARCHIVE_HOME,
//     CLAUDE_CONFIG_DIR, CODEX_HOME and the AWS configuration variables are
//     unset, so nothing that resolves a default location lands in the
//     developer's own ~/.claude, ~/.cursor, ~/.local/share/agent-archive,
//     or ~/.aws.
//
//   - $TMPDIR, and so every t.TempDir: a fresh folder of the run's own under
//     /tmp, rather than macOS's per-user temporary folder.
//
// It returns a function that removes the temporary folders.
func isolateProcessForTesting() func() {
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	parent := ""
	if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
		parent = "/tmp"
	}
	tmp, err := os.MkdirTemp(parent, testTempPrefix)
	must(err)
	must(os.Setenv("TMPDIR", tmp))
	home, err := os.MkdirTemp("", "capture-home-")
	must(err)
	must(os.Setenv("HOME", home))
	for _, name := range []string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE"} {
		must(os.Unsetenv(name))
	}
	return func() { _ = os.RemoveAll(home); _ = os.RemoveAll(tmp) }
}

// TestIsolationFailsClosed pins isolateProcessForTesting: the process's own
// home and temporary folder are the run's, and no variable that moves a data
// or app directory leaks in.
func TestIsolationFailsClosed(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if account, err := user.Current(); err == nil && filepath.Clean(account.HomeDir) == filepath.Clean(home) {
		t.Fatalf("tests run with the account's real home %s", home)
	}
	for _, name := range []string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s=%s leaks into the tests", name, value)
		}
	}
	if dir := t.TempDir(); !strings.Contains(dir, testTempPrefix) {
		t.Errorf("t.TempDir() = %s, outside this run's temporary folder", dir)
	}
}
