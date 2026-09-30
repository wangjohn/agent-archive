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
	"fmt"
	"os/exec"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// Default is the scheduler of the system this process runs on, over the real
// Runner (Exec).
func Default() scheduler.Scheduler { return New(platform.Current(), Exec) }

// New is the scheduler for system, running its tool through run: launchd on
// macOS. Another system has none yet: its scheduler says every job's state is
// unknown and refuses to load or stop one, so nothing is changed on a system
// the program cannot manage.
func New(system platform.OS, run scheduler.Runner) scheduler.Scheduler {
	if system == platform.Darwin {
		return launchd.Scheduler{Run: run}
	}
	return unsupported{system}
}

// waitDelay is how long Exec waits for a program's output after its context
// ends and the program is killed.
const waitDelay = 2 * time.Second

// Exec is the real Runner: name found on PATH, run with args, in a process
// group of its own, returning its combined output. A terminal's Ctrl-C reaches
// setup, which decides what to do, not a launchctl halfway through changing a
// job.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	ownProcessGroup(cmd)
	// A child that outlives the kill must not keep this waiting on its pipe.
	cmd.WaitDelay = waitDelay
	return cmd.CombinedOutput()
}

// unsupported is the scheduler of a system with no adapter yet.
type unsupported struct{ system platform.OS }

func (unsupported) JobState(context.Context, scheduler.Site, scheduler.Ref) scheduler.JobState {
	return scheduler.Unknown
}

func (u unsupported) Load(context.Context, scheduler.Site, scheduler.Ref) error { return u.refuse() }

func (u unsupported) Unload(context.Context, scheduler.Site, scheduler.Ref) error { return u.refuse() }

func (u unsupported) refuse() error {
	return fmt.Errorf("no scheduler for this system yet (%s); the background collector was left as it is", u.system)
}
