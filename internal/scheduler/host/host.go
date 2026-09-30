// Package host chooses the scheduler for the system the program runs on and
// owns the Runner that really runs its tool. It is the only package that
// imports the adapters under internal/scheduler (TestOnlyHostImportsAdapters),
// so everything else names the port alone.
//
// Nothing here runs a program until a job is asked about or changed: the hook
// runtime has a two-second budget, and constructing a scheduler must not
// spend it.
package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
)

// The adapters' names, which a setup journal records (Definer.Name) and a
// config.json may (see Recorded).
const (
	launchdName = "launchd"
	systemdName = "systemd"
	// noneName is the scheduler of a system with no adapter.
	noneName = "none"
)

// New is the scheduler for system, running its tool through run: launchd on
// macOS and systemd (the user's own manager) on Linux. Another system has
// none: its scheduler, called "none", says every job's state is unknown and
// refuses to load or stop one, so nothing is changed on a system the program
// cannot manage.
func New(system platform.OS, run scheduler.Runner) scheduler.Scheduler {
	switch system {
	case platform.Darwin:
		return launchd.Scheduler{Run: run}
	case platform.Linux:
		return systemd.Scheduler{Run: run}
	case platform.Unknown:
	}
	return Unavailable(noneName, fmt.Sprintf("agent-archive has no background scheduler for this system (%s)", system), "Run agent-archive on macOS, or on Linux with a systemd user manager: its background collector runs under launchd or systemd")
}

// Named is the scheduler called name on system, over run: the one a
// config.json records (config.Config.BackgroundBackend) or a setup journal
// does (setupjournal.Journal.Backend). The empty name is the system's own, as
// New chooses it, which is what a configuration that records none means:
// launchd on macOS and systemd on Linux, for all time. A name this build has
// no adapter for, or one whose manager this system does not have (launchd on
// Linux), is an error: a job it made is not one this system can ask about.
func Named(system platform.OS, name string, run scheduler.Runner) (scheduler.Scheduler, error) {
	switch {
	case name == "", name == launchdName && system == platform.Darwin, name == systemdName && system == platform.Linux:
		return New(system, run), nil
	case name == launchdName, name == systemdName:
		return nil, fmt.Errorf("the %s scheduler is not available on this system (%s)", name, system)
	}
	return nil, fmt.Errorf("this agent-archive does not know a %q scheduler", name)
}

// Lookup is Named for the system this process runs on, over the real Runner
// (Exec): Lookup("") is this system's own scheduler.
func Lookup(name string) (scheduler.Scheduler, error) { return Named(platform.Current(), name, Exec) }

// Recorded is what setup writes in config.json to name the backend it used:
// the backend's name, except that launchd is left out. A configuration that
// says nothing means launchd on macOS and systemd on Linux for all time, and
// every configuration written before the field existed is macOS's, so leaving
// launchd out keeps each macOS config.json byte for byte what it was.
func Recorded(name string) string {
	if name == launchdName {
		return ""
	}
	return name
}

// waitDelay is how long Exec waits for a program's output after its context
// ends and the program is killed.
const waitDelay = 2 * time.Second

// Exec is the real Runner: name found on PATH, run with args, in a process
// group of its own, returning its combined output. A terminal's Ctrl-C reaches
// setup, which decides what to do, not a launchctl halfway through changing a
// job. systemctl and loginctl get the environment they need to reach the
// user's manager (see managerEnvironment).
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	ownProcessGroup(cmd)
	// A child that outlives the kill must not keep this waiting on its pipe.
	cmd.WaitDelay = waitDelay
	if talksToUserManager[name] {
		cmd.Env = managerEnvironment(os.Environ())
	}
	return cmd.CombinedOutput()
}

// talksToUserManager are the programs the systemd adapter runs.
var talksToUserManager = map[string]bool{"systemctl": true, "loginctl": true}

// managerEnvironment is environ for a program that talks to the user's systemd
// manager: the variables that say where it is, XDG_RUNTIME_DIR and
// DBUS_SESSION_BUS_ADDRESS, passed on as this process has them (systemctl
// --user finds the bus from the first; a process with neither is told there
// is no user bus, and the adapter says how to get one), and SYSTEMD_COLORS=0,
// so color codes never get into the version line or the properties the
// adapter parses, whatever the terminal or the user's environment says.
func managerEnvironment(environ []string) []string {
	env := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, "SYSTEMD_COLORS=") {
			env = append(env, entry)
		}
	}
	return append(env, "SYSTEMD_COLORS=0")
}

// Unavailable is a scheduler called name that cannot be used: the one of a
// system with no adapter ("none"), or of a backend a configuration names that
// this system cannot reach (why says so, and fix what to do about it). It
// defines no job, says every job's state is unknown with why and fix as the
// problem, and refuses to load or stop one, so a command refuses to go on
// rather than act on a job it cannot see.
func Unavailable(name, why, fix string) scheduler.Scheduler {
	return unavailable{name: name, why: why, fix: fix}
}

// unavailable is the scheduler Unavailable makes.
type unavailable struct {
	name string
	why  string
	fix  string
}

// unavailableRef is the job's name under a scheduler that cannot be used: there
// is no adapter to say what a job is called, and nothing is written for it.
const unavailableRef scheduler.Ref = "agent-archive-collector"

func (u unavailable) Name() string { return u.name }

func (unavailable) Words() scheduler.Words {
	return scheduler.Words{Manager: "the background scheduler", Job: "background job", Definition: "job definition", Tool: "the scheduler", Name: "name"}
}

func (unavailable) Ref(scheduler.Installation) scheduler.Ref { return unavailableRef }

func (unavailable) DefaultPATH() string { return "/usr/local/bin:/usr/bin:/bin" }

func (u unavailable) Plan(scheduler.Site, scheduler.Installation, scheduler.JobSpec) (scheduler.Plan, error) {
	return scheduler.Plan{}, errors.New(u.why)
}

func (u unavailable) Locate(definition string) (scheduler.Site, scheduler.Ref, error) {
	return scheduler.Site{}, "", fmt.Errorf("%s is not a definition this system can place: %s", definition, u.why)
}

func (unavailable) Definition(scheduler.Site, scheduler.Ref) scheduler.Status {
	return scheduler.Status{}
}

func (unavailable) Installed(context.Context, scheduler.Site, scheduler.Installation) ([]scheduler.Job, error) {
	return []scheduler.Job{{Ref: unavailableRef}}, nil
}

func (u unavailable) Inspect(_ context.Context, _ scheduler.Site, ref scheduler.Ref) scheduler.Status {
	return scheduler.Status{State: scheduler.Unknown, Problem: &scheduler.Problem{Kind: scheduler.ProblemCannotTell, Ref: ref, Reason: u.why, Fix: u.fix}}
}

func (u unavailable) Load(context.Context, scheduler.Site, scheduler.Ref) error { return u.refuse() }

func (u unavailable) Unload(context.Context, scheduler.Site, scheduler.Ref) error { return u.refuse() }

func (u unavailable) refuse() error {
	return fmt.Errorf("%s; the background collector was left as it is", u.why)
}
