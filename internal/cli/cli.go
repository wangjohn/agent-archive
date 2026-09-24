// Package cli implements the agent-archive command-line interface: the
// hidden `_hook` and `_collect` entry points a real installation's hooks
// and LaunchAgent invoke, and the user-facing
// setup/status/sync/pause/resume/uninstall, read-only list/show, and
// handoff commands. It is the only package that touches process-level state
// (args, stdio, the real clock, the real home directory) directly; every
// other package in this module stays free of that so it can be tested
// without a real environment.
package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Version is the released version string. main overrides it via
// -ldflags "-X .../cli.Version=..." at build time (see the Distribution PR);
// it stays "dev" for a local build.
var Version = "dev"

// Env carries the process-level dependencies a command needs, so tests can
// substitute a temporary home directory, a fixed clock, and an in-memory
// object store. A nil field defaults to the real thing.
type Env struct {
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
	// out to launchctl; unverified against a real launchd (see the
	// implementation ledger).
	LoadLaunchAgent func(plistPath string) error
	// UnloadLaunchAgent undoes a successful LoadLaunchAgent: it rolls setup
	// back if a later step (config.Save) fails after the LaunchAgent was
	// already loaded, and stops the collector during uninstall. Defaults to
	// shelling out to launchctl; unverified against a real launchd, same as
	// LoadLaunchAgent.
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
	// Interrupts delivers Ctrl-C while backfill uploads, and stop ends the
	// delivery. Defaults to os/signal for os.Interrupt.
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
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
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
	return openConfiguredStore(cfg)
}

func (e Env) executable() (string, error) {
	if e.Executable != nil {
		return e.Executable()
	}
	return os.Executable()
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
	return detectHarnesses(userHome)
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
	return credentials.NewKeychainStore(credentials.KeychainService)
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
  agent-archive show ID     Read a session's metadata
  agent-archive feedback ID Add explicit feedback from a local file

Import history
  agent-archive backfill    Import sessions already on this Mac

Switch agents
  agent-archive handoff     Continue a session in another coding agent

Maintenance
  agent-archive uninstall   Remove integrations; keep local data

Run agent-archive COMMAND --help for options and examples.
Use --version to show the installed version.
You supply a private Cloudflare R2 or Amazon S3 bucket. No archive account needed.
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
		fmt.Fprint(stdout, usage)
		return 0
	}
	if handled, code := commandPreflight(args, stdout, stderr); handled {
		return code
	}

	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "-v", "--version", "version":
		fmt.Fprintln(stdout, Version)
		return 0
	case "_hook":
		return runHookCommand(args[1:], stdin, stderr, env)
	case "_collect":
		return runCollectCommand(args[1:], stdout, stderr, env)
	case "status":
		return runStatusCommand(args[1:], stdout, stderr, env)
	case "sync":
		return runSyncCommand(args[1:], stdout, stderr, env)
	case "pause":
		return runPauseCommand(stdout, stderr, env, true)
	case "resume":
		return runPauseCommand(stdout, stderr, env, false)
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
		fmt.Fprintf(stderr, "agent-archive: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
