package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// testTempPrefix names the folder under /tmp that holds one test run's
// temporary files.
const testTempPrefix = "agent-archive-cli-test-"

// isolateProcessForTesting makes the package's tests fail closed: a test
// that leaves an Env field nil gets the default, and every default that
// reaches past the test's temporary directories is replaced here, before any
// test runs, with one that stays inside them or stops the test.
//
//   - launchctl: runLaunchctl panics, naming the command. launchd is global to
//     the login session, so even `launchctl print` from a test reads the
//     developer's real jobs, and bootstrap or bootout would change them. A
//     test that means to drive launchctl stubs it with stubLaunchctl.
//
//   - The credential store: openCredentialStore panics, so neither the real
//     Keychain nor a credentials file in a real data directory can be
//     reached. Set Env.Credentials (newFakeKeychain).
//
//   - The platform the credential store is named for: credentialGOOS is
//     "darwin", so the many tests whose fake stands for the Keychain see the
//     Keychain's wording on every runner, Linux CI included. A test of the
//     other platform's wording sets it to "linux" (useCredentialGOOS).
//
//   - less: detectLessVersion panics. Set Env.LessVersion (testEnv does).
//
//   - The terminal's modes: openTerminalKeys never reads keys, so the
//     session browser reads lines, and no test changes the modes of the
//     terminal running it. Set Env.openKeys to read keys.
//
//   - $HOME and the variables that move app and data directories: HOME is a
//     fresh temporary directory, and AGENT_ARCHIVE_HOME, CLAUDE_CONFIG_DIR,
//     CODEX_HOME and the AWS configuration variables are unset, so
//     os.UserHomeDir, local.Home, and the AWS SDK's profile files all land
//     there rather than in the developer's own ~/.claude, ~/.cursor,
//     ~/.local/share/agent-archive, or ~/.aws.
//
//   - $TMPDIR, and so every t.TempDir: a fresh folder of the run's own under
//     /tmp. local.CanonicalPath lists every parent of a path to find its
//     spelling, and macOS's per-user temporary folder can hold thousands of
//     entries; listing it for every path was most of this package's run time.
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
	// A test binary this one starts (the terminal tests' child) inherits
	// $TMPDIR and nests its folder there, so this run's removal covers it
	// even when the child is killed before it can clean up.
	if inherited := os.Getenv("TMPDIR"); strings.HasPrefix(filepath.Base(filepath.Clean(inherited)), testTempPrefix) {
		parent = inherited
	}
	tmp, err := os.MkdirTemp(parent, testTempPrefix)
	must(err)
	must(os.Setenv("TMPDIR", tmp))
	home, err := os.MkdirTemp("", "cli-home-")
	must(err)
	must(os.Setenv("HOME", home))
	// Nor does a test see the agent it may be run from: an agent's variables
	// switch off every prompt. Tests that mean an agent inject them through
	// Env.LookupEnv, and the suite is also run with them set to prove it.
	for _, name := range append([]string{"AGENT_ARCHIVE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_PROFILE", "XDG_CONFIG_HOME", envNonInteractive}, agentShellEnv()...) {
		must(os.Unsetenv(name))
	}
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		panic(fmt.Sprintf("a test reached the real launchctl %q: set Env.JobState, Env.LoadLaunchAgent and Env.UnloadLaunchAgent (testEnv does), or call stubLaunchctl", args))
	}
	realOpenCredentialStore = openCredentialStore
	openCredentialStore = func() (credentials.CredentialStore, error) {
		panic("a test reached the real credential store: set Env.Credentials (newFakeKeychain)")
	}
	productionCredentialGOOS = credentialGOOS
	credentialGOOS = "darwin"
	openAWSBuckets = func(string, string) (BucketFinder, error) {
		return nil, errors.New("no AWS in this test: set Env.AWSBuckets")
	}
	detectLessVersion = func(string) (int, bool) {
		panic("a test reached the real less: set Env.LessVersion (testEnv does)")
	}
	openTerminalKeys = func(io.Reader) (keyTerminal, bool) { return nil, false }
	return func() { _ = os.RemoveAll(home); _ = os.RemoveAll(tmp) }
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
	panics("Env{}.credentialStore", func() { _, _ = Env{}.credentialStore() })
	panics("less --version", func() { _, _ = Env{}.lessVersion("less") })
	panics("R2 store", func() {
		_, _ = Env{}.openStore(config.Config{Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2CredentialRef: "r"}})
	})
	if _, err := (Env{}).awsBuckets("default", "us-east-1"); err == nil {
		t.Error("Env{}.awsBuckets reached AWS instead of failing")
	}
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
	if _, err := env.credentialStore(); err == nil {
		t.Error("testEnv's credential store must fail unless a test sets one")
	}
	if _, err := env.executable(); err == nil {
		t.Error("testEnv's Executable must fail unless a test sets one")
	}
	if got := env.jobState("/nonexistent.plist"); got != "missing" {
		t.Errorf("testEnv job state = %q", got)
	}
}

// realOpenCredentialStore is the default openCredentialStore, which
// isolateProcessForTesting replaced, kept so a test can check how it is wired
// (TestOpenCredentialStoreIsWiredToTheDataDirectory).
var realOpenCredentialStore func() (credentials.CredentialStore, error)

// productionCredentialGOOS is credentialGOOS as the program starts, before
// isolateProcessForTesting pins it to "darwin" for the tests.
var productionCredentialGOOS string

// useCredentialGOOS names the credential store for another platform for one
// test. The test must not be parallel: the variable is shared.
func useCredentialGOOS(t *testing.T, goos string) {
	t.Helper()
	previous := credentialGOOS
	credentialGOOS = goos
	t.Cleanup(func() { credentialGOOS = previous })
}

// stubLaunchctl replaces launchctl for one test.
func stubLaunchctl(t *testing.T, run func(args ...string) ([]byte, error)) {
	t.Helper()
	previous := runLaunchctl
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) { return run(args...) }
	t.Cleanup(func() { runLaunchctl = previous })
}
