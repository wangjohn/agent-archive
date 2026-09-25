// Package cli implements the agent-archive command-line interface: the
// hidden `_hook` and `_collect` entry points a real installation's hooks
// and LaunchAgent invoke, and the user-facing
// setup/status/sync/pause/resume/uninstall, read-only list/show, and
// handoff commands. Process-level state (args, stdio, the clock, the home
// directory, launchctl, the Keychain) reaches commands through Env, so a test
// can substitute every piece of it; a nil Env field means the real thing.
// A few lower packages still read the process directly: local resolves the
// data directory from AGENT_ARCHIVE_HOME and $HOME, credentials reads the AWS
// configuration files and the Keychain, and cursorstore asks getconf for the
// user's temporary directory.
package cli

import (
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Version is the released version string. scripts/build-release.sh sets it
// with -ldflags "-X .../cli.Version=..."; it stays "dev" for a local build,
// which --version then reports with its commit (see versionString).
var Version = "dev"

// versionString is what --version prints: Version, or for a "dev" build the
// commit it was built from, so a bug report from a source build says which
// code it ran.
func versionString() string {
	info, _ := debug.ReadBuildInfo()
	return describeVersion(Version, info)
}

// describeVersion returns version unchanged unless it is "dev" and info
// records a VCS revision; then it is "dev-<first 12 hex of the commit>", with
// "-dirty" when the working tree had uncommitted changes.
func describeVersion(version string, info *debug.BuildInfo) string {
	if version != "dev" || info == nil {
		return version
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		//lint:ignore LV1001 build setting keys are arbitrary text; only these two matter
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return version
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	version += "-" + revision
	if modified {
		version += "-dirty"
	}
	return version
}

// Env carries the process-level dependencies a command needs, so tests can
// substitute a temporary home directory, a fixed clock, and an in-memory
// object store. A nil field defaults to the real thing.
type Env struct {
	// sweepClock, set only by tests, adjusts the retention sweep's clock
	// checks (retention.Options.ServerClock and PreviousScanAt). A test that
	// moves Now months ahead moves only this Mac's clock; the sweep rightly
	// refuses to delete by it unless the storage clock moves too.
	sweepClock  func(*retention.Options)
	AWSProfiles func() ([]AWSProfile, error)
	WorkingDir  func() (string, error)
	// JobState reports loaded, running, missing, or unknown without changing launchd.
	JobState func(string) string
	Home     func() (string, error)
	Now      func() time.Time
	// OpenStore builds the object store a collector pass publishes to, from
	// this machine's configured storage destination. Defaults to
	// openConfiguredStore, which resolves real AWS/R2 credentials.
	OpenStore func(config.Config) (storage.ObjectStore, error)
	// Executable returns the absolute path setup installs into hook
	// commands and the LaunchAgent. Defaults to os.Executable.
	Executable func() (string, error)
	// UserHomeDir is the real user home directory — where hook config files
	// and ~/Library/LaunchAgents live — as distinct from Home, which is
	// agent-archive's own (possibly redirected) private data directory.
	// Defaults to os.UserHomeDir.
	UserHomeDir func() (string, error)
	// AccountHome is the account's home directory from the user database,
	// which overriding $HOME does not change. Only the data directory under
	// it is the default installation, with the default launchd label.
	// Defaults to os/user.Current's HomeDir.
	AccountHome func() (string, error)
	// DetectHarnesses best-effort detects which applications appear
	// installed under a user home directory, to pre-select setup's
	// application prompts; the user can still include or exclude any of
	// them regardless of what this reports. Defaults to detectHarnesses.
	DetectHarnesses func(userHome string) []string
	// DiscoverApplications performs bounded, read-only installed-version
	// discovery. It must not inspect transcripts, install hooks, or use the network.
	DiscoverApplications func(userHome string) map[string]applicationDiscovery
	// LoadLaunchAgent loads the just-written LaunchAgent plist so scheduled
	// collection starts without a login/logout cycle. Defaults to shelling
	// out to launchctl (runLaunchctl).
	LoadLaunchAgent func(plistPath string) error
	// UnloadLaunchAgent undoes a successful LoadLaunchAgent: it rolls setup
	// back if a later step (config.Save) fails after the LaunchAgent was
	// already loaded, and stops the collector during uninstall. Defaults to
	// shelling out to launchctl, like LoadLaunchAgent.
	UnloadLaunchAgent func(plistPath string) error
	// Keychain opens the credential store setup saves R2 secrets to and
	// uninstall deletes them from.
	// Defaults to credentials.NewKeychainStore, which is only available on
	// a darwin+cgo build.
	Keychain func() (credentials.CredentialStore, error)
	// LookupEnv reads the process environment. `handoff --latest` uses it to
	// recognize the agent session it is running inside. Defaults to
	// os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// BackfillTempDirs are the temporary directories backfill skips. Nil
	// means the macOS defaults plus $TMPDIR; tests set it because their
	// files live in one.
	BackfillTempDirs []string
	// IsTerminal reports whether stdin or stdout is an interactive
	// terminal. backfill asks for confirmation only on one, and redraws its
	// progress line only on one. Defaults to checking the file descriptor.
	IsTerminal func(any) bool
	// Interrupts delivers the signals that stop backfill while it plans,
	// registers, and uploads, and stop ends the delivery. Defaults to
	// os/signal for os.Interrupt, SIGTERM, and SIGHUP.
	Interrupts func() (signals <-chan os.Signal, stop func())
}

func (e Env) isTerminal(stream any) bool {
	if e.IsTerminal != nil {
		return e.IsTerminal(stream)
	}
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (e Env) interrupts() (<-chan os.Signal, func()) {
	if e.Interrupts != nil {
		return e.Interrupts()
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	return signals, func() { signal.Stop(signals) }
}

func (e Env) lookupEnv(key string) (string, bool) {
	if e.LookupEnv != nil {
		return e.LookupEnv(key)
	}
	return os.LookupEnv(key)
}

func (e Env) home() (string, error) {
	if e.Home != nil {
		return e.Home()
	}
	return local.Home()
}

func (e Env) readHome() (string, error) {
	if e.Home != nil {
		return e.Home()
	}
	return local.ReadHome()
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) openStore(cfg config.Config) (storage.ObjectStore, error) {
	if e.OpenStore != nil {
		return e.OpenStore(cfg)
	}
	return openConfiguredStore(cfg, e.keychain)
}

func (e Env) executable() (string, error) {
	if e.Executable != nil {
		return e.Executable()
	}
	return os.Executable()
}

// accountHome is Env.AccountHome, or "" when it cannot be read, in which
// case no installation counts as the default one.
func (e Env) accountHome() string {
	lookup := e.AccountHome
	if lookup == nil {
		lookup = func() (string, error) {
			u, err := user.Current()
			if err != nil {
				return "", err
			}
			return u.HomeDir, nil
		}
	}
	home, err := lookup()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	return home
}

func (e Env) userHomeDir() (string, error) {
	if e.UserHomeDir != nil {
		return e.UserHomeDir()
	}
	return os.UserHomeDir()
}

// cursorDatabase is Cursor's state.vscdb under the user's home, which
// cursor-sqlite sessions are read from; "" (the process's own home) only
// when the home can't be resolved.
func (e Env) cursorDatabase() string {
	home, err := e.userHomeDir()
	if err != nil {
		return ""
	}
	return cursorstore.StateDatabase(home)
}

func (e Env) detectHarnesses(userHome string) []string {
	if e.DetectHarnesses != nil {
		return e.DetectHarnesses(userHome)
	}
	return detectHarnesses(e.hookFiles(userHome))
}

func (e Env) discoverApplications(userHome string) map[string]applicationDiscovery {
	if e.DiscoverApplications != nil {
		return e.DiscoverApplications(userHome)
	}
	return discoverApplications(userHome)
}

func (e Env) loadLaunchAgent(plistPath string) error {
	if e.LoadLaunchAgent != nil {
		return e.LoadLaunchAgent(plistPath)
	}
	return loadLaunchAgent(plistPath)
}

func (e Env) unloadLaunchAgent(plistPath string) error {
	if e.UnloadLaunchAgent != nil {
		return e.UnloadLaunchAgent(plistPath)
	}
	return unloadLaunchAgent(plistPath)
}

func (e Env) keychain() (credentials.CredentialStore, error) {
	if e.Keychain != nil {
		return e.Keychain()
	}
	return openKeychain()
}

// openKeychain opens the login Keychain's agent-archive items: Env.Keychain's
// default. The package's tests replace it with one that fails the test, so a
// test that forgets to set Env.Keychain can never reach the real Keychain.
var openKeychain = func() (credentials.CredentialStore, error) {
	store, err := credentials.NewKeychainStore(credentials.KeychainService)
	if err != nil {
		// Never a non-nil interface holding a nil store.
		return nil, err
	}
	return store, nil
}

const usage = `Agent Archive — archive coding-agent sessions to your private storage.

Get started
  agent-archive setup       Configure apps, projects, and storage
  agent-archive status      Check capture and see what to do next

Manage capture
  agent-archive sync        Collect and upload pending changes now
  agent-archive pause       Pause collection, uploads, and cleanup
  agent-archive resume      Resume automatic capture

Inspect history
  agent-archive list        Find archived sessions
  agent-archive show        Read a session's metadata
  agent-archive feedback    Add explicit feedback from a local file

Import history
  agent-archive backfill    Import sessions already on this Mac

Switch agents
  agent-archive handoff     Continue a session in another coding agent

Maintenance
  agent-archive uninstall   Remove integrations; keep local data

Run agent-archive COMMAND --help for options and examples.
Use --version to show the installed version.
You supply a private Cloudflare R2 or Amazon S3 bucket. No archive account needed.
Docs: https://github.com/wangjohn/agent-archive/tree/main/docs
`

// Run dispatches one CLI invocation and returns a process exit code. It
// never panics on malformed input; every command reports a problem through
// stderr and a nonzero exit code instead.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if len(args) == 0 {
		terminal.Print(stdout, usage)
		return 0
	}
	if handled, code := commandPreflight(args, stdout, stderr); handled {
		return code
	}

	switch args[0] {
	case "-h", "--help", "help":
		terminal.Print(stdout, usage)
		return 0
	case "-v", "--version", "version":
		terminal.Println(stdout, versionString())
		return 0
	case "_hook":
		return runHookCommand(args[1:], stdin, stderr, env)
	case "_collect":
		return runCollectCommand(args[1:], stdout, stderr, env)
	case "status":
		return runStatusCommand(args[1:], stdout, stderr, env)
	case "sync":
		return runSyncCommand(args[1:], stdout, stderr, env)
	case "pause", "resume":
		if !newCommandFlags(args[0], stderr).parseFlagsOnly(args[1:]) {
			return 2
		}
		return runPauseCommand(stdout, stderr, env, args[0] == "pause")
	case "setup":
		return runSetupCommand(args[1:], stdin, stdout, stderr, env)
	case "uninstall":
		return runUninstallCommand(args[1:], stdin, stdout, stderr, env)
	case "list":
		return runListCommand(args[1:], stdout, stderr, env)
	case "show":
		return runShowCommand(args[1:], stdout, stderr, env)
	case "feedback":
		return runFeedbackCommand(args[1:], stdout, stderr, env)
	case "handoff":
		return runHandoffCommand(args[1:], stdout, stderr, env)
	case "backfill":
		return runBackfillCommand(args[1:], stdin, stdout, stderr, env)
	default:
		terminal.Printf(stderr, "agent-archive: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
