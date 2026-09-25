package cli

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// isolateProcessForTesting makes the package's tests fail closed: a test
// that leaves an Env field nil gets the default, and every default that
// reaches past the test's temporary directories is replaced here, before any
// test runs, with one that stays inside them or stops the test.
//
//   - launchctl: runLaunchctl panics, naming the command. launchd is global to
//     the login session, so even `launchctl print` from a test reads the
//     developer's real jobs, and bootstrap or bootout would change them. A
//     test that means to drive launchctl stubs it with stubLaunchctl.
//   - The Keychain: openKeychain panics. Set Env.Keychain (newFakeKeychain).
//   - $HOME and the variables that move app and data directories: HOME is a
//     fresh temporary directory, and AGENT_ARCHIVE_HOME, CLAUDE_CONFIG_DIR,
//     CODEX_HOME and the AWS configuration variables are unset, so
//     os.UserHomeDir, local.Home, and the AWS SDK's profile files all land
//     there rather than in the developer's own ~/.claude, ~/.cursor,
//     ~/.local/share/agent-archive, or ~/.aws.
//
// It returns a function that removes the temporary home.
func isolateProcessForTesting() func() {
	home, err := os.MkdirTemp("", "cli-home-")
	if err != nil {
		panic(err)
	}
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(os.Setenv("HOME", home))
	for _, name := range []string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE"} {
		must(os.Unsetenv(name))
	}
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		panic(fmt.Sprintf("a test reached the real launchctl %q: set Env.JobState, Env.LoadLaunchAgent and Env.UnloadLaunchAgent (testEnv does), or call stubLaunchctl", args))
	}
	openKeychain = func() (credentials.CredentialStore, error) {
		panic("a test reached the real Keychain: set Env.Keychain (newFakeKeychain)")
	}
	return func() { _ = os.RemoveAll(home) }
}

// TestIsolationFailsClosed pins isolateProcessForTesting: every default that
// reaches this Mac itself stops the test, and the process's own home is a
// temporary one.
func TestIsolationFailsClosed(t *testing.T) {
	panics := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s reached the real system instead of stopping the test", name)
			}
		}()
		f()
	}
	panics("launchctl print", func() { launchdJobState("/nonexistent/com.agent-archive.collector.plist") })
	panics("launchctl bootstrap", func() { _ = loadLaunchAgent("/nonexistent/x.plist") })
	panics("Env{}.keychain", func() { _, _ = Env{}.keychain() })
	panics("R2 store", func() {
		_, _ = Env{}.openStore(config.Config{Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2CredentialRef: "r"}})
	})
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if account, err := user.Current(); err == nil && filepath.Clean(account.HomeDir) == filepath.Clean(home) {
		t.Fatalf("tests run with the account's real home %s", home)
	}
	for _, name := range []string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s=%s leaks into the tests", name, value)
		}
	}
	// testEnv's side-effecting fields fail rather than reach the Mac.
	env := testEnv(t, t.TempDir(), time.Now())
	if _, err := env.keychain(); err == nil {
		t.Error("testEnv's Keychain must fail unless a test sets one")
	}
	if _, err := env.executable(); err == nil {
		t.Error("testEnv's Executable must fail unless a test sets one")
	}
	if got := env.jobState("/nonexistent.plist"); got != "missing" {
		t.Errorf("testEnv job state = %q", got)
	}
}
