// Package scheduler is the port between agent-archive and whatever runs its
// background collector: launchd on macOS today, and another manager for
// another system as its adapter arrives. It holds the types the commands
// speak in (a job's Ref, where its definitions live, its JobState) and the
// interface every adapter implements, and nothing that runs a program: an
// adapter runs its manager's tool through a Runner it is given, so this
// package and everything that only names it is pure
// (TestSchedulerImportBoundary). The adapters live under this directory
// (internal/scheduler/launchd), and internal/scheduler/host chooses one for
// the system the program runs on.
package scheduler

import "context"

// Ref names one background job to its scheduler. It is opaque to callers and
// minted by the adapter's own vocabulary: for launchd, the job's label.
type Ref string

// Site is where a scheduler looks for job definitions: the user's own home,
// which is not the data directory. It is per call, not per scheduler, because
// a setup interrupted under one $HOME is recovered under another (a sandbox
// overrides $HOME, and so the definitions' directory, but not the manager).
type Site struct{ UserHome string }

// JobState is what a scheduler says about a job. The values are what
// `status --json` reports as `background` (which adds "broken" of its own),
// and what setup's journal records.
type JobState string

const (
	// Loaded is a job the scheduler has loaded from this site's own
	// definition and is not running at the moment.
	Loaded JobState = "loaded"
	// Running is a loaded job that is running now.
	Running JobState = "running"
	// Missing is a job the scheduler does not have loaded.
	Missing JobState = "missing"
	// Unknown is a job the scheduler cannot say anything about: the manager
	// is unreachable, or its answer names no definition to compare.
	Unknown JobState = "unknown"
	// AnotherInstallation is a job of the same name that the scheduler loaded
	// from another definition, which belongs to another installation.
	AnotherInstallation JobState = "another_installation"
)

// Scheduler is the background job manager as every command uses it. A test
// stands in with a fake; the real one for this system is host.Default.
//
// JobState is short (launchd: 2 s). Load and Unload run on a bounded context
// of their own (launchd: 30 s) that ctx's cancellation never reaches, so an
// interrupt never stops a change halfway.
type Scheduler interface {
	// JobState says whether the job ref names is loaded, without changing
	// anything.
	JobState(ctx context.Context, site Site, ref Ref) JobState
	// Load loads (bootstraps) the definition on disk for ref, so scheduled
	// collection starts without a login.
	Load(ctx context.Context, site Site, ref Ref) error
	// Unload stops the job ref names, only when the scheduler loaded it from
	// this site's own definition: nil when it is not loaded, and an error,
	// stopping nothing, when it belongs to another installation or the
	// scheduler cannot say.
	Unload(ctx context.Context, site Site, ref Ref) error
}

// Runner runs a program and returns its combined standard output and
// standard error, as exec.Cmd.CombinedOutput does: an adapter reads a failed
// command's words from that output (launchctl print's "Could not find
// service"), and quotes it in the errors it returns. Tests replace the Runner
// an adapter is given, so no test reaches the real manager.
type Runner func(ctx context.Context, name string, args ...string) (output []byte, err error)
