package cli

import (
	"context"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// schedulerRef names one background job to the scheduler. For launchd it is
// the job's label, which is what launchLabel reads off a plist path. Callers
// that still hold a plist path (they read or write its file) name the job with
// jobRef.
type schedulerRef string

// schedulerSite is where the scheduler looks for job definitions: the user's
// own home, which is not the data directory.
type schedulerSite struct{ userHome string }

// scheduler is the background job manager as every command uses it: launchd
// today. A test stands in with fakeScheduler; Env.Scheduler nil means the
// real one (launchdScheduler).
//
// The states are the strings status reports (jobState's result, which
// setupjournal.JobActive and JobAnotherInstallation read): "loaded",
// "running", "missing", "unknown", or setupjournal.JobAnotherInstallation.
type scheduler interface {
	// jobState says whether the job ref names is loaded, without changing
	// anything.
	jobState(ctx context.Context, site schedulerSite, ref schedulerRef) string
	// load loads (bootstraps) the definition on disk for ref, so scheduled
	// collection starts without a login.
	load(ctx context.Context, site schedulerSite, ref schedulerRef) error
	// unload stops the job ref names, only when the scheduler loaded it from
	// this site's own definition: nil when it is not loaded, and an error,
	// stopping nothing, when it belongs to another installation or the
	// scheduler cannot say.
	unload(ctx context.Context, site schedulerSite, ref schedulerRef) error
}

// launchdScheduler is launchd, through launchctl (runLaunchctl). A job's
// definition is <user home>/Library/LaunchAgents/<label>.plist, which is where
// every collector plist, earlier release's plist and the prototype's job
// lives.
type launchdScheduler struct{}

func (launchdScheduler) jobState(ctx context.Context, site schedulerSite, ref schedulerRef) string {
	return launchdJobState(ctx, site.launchAgent(ref))
}

func (launchdScheduler) load(ctx context.Context, site schedulerSite, ref schedulerRef) error {
	return loadLaunchAgent(ctx, site.launchAgent(ref))
}

func (launchdScheduler) unload(ctx context.Context, site schedulerSite, ref schedulerRef) error {
	return unloadLaunchAgent(ctx, site.launchAgent(ref))
}

// launchAgent is the plist that defines the job ref names.
func (s schedulerSite) launchAgent(ref schedulerRef) string {
	return filepath.Join(s.userHome, "Library", "LaunchAgents", string(ref)+".plist")
}

// jobRef is the job the plist at path defines.
func jobRef(plist string) schedulerRef { return schedulerRef(launchLabel(plist)) }

// scheduler is e's job scheduler: Env.Scheduler, or launchd itself.
func (e Env) scheduler() scheduler {
	if e.Scheduler != nil {
		return e.Scheduler
	}
	return launchdScheduler{}
}

// jobState is the state of the job plist defines, for callers that hold the
// plist's path.
func (e Env) jobState(userHome, plist string) string {
	return e.scheduler().jobState(context.Background(), schedulerSite{userHome}, jobRef(plist))
}

// unloadJob stops the job plist defines (see scheduler.unload).
func (e Env) unloadJob(userHome, plist string) error {
	return e.scheduler().unload(context.Background(), schedulerSite{userHome}, jobRef(plist))
}

// launchd is e's scheduler as internal/setupjournal drives it: the same one
// every command uses, so a test's stand-in (and TestMain's failing launchctl)
// applies there too. setupjournal names jobs by plist path, so each call reads
// the job's ref off the path.
func (e Env) launchd(userHome string) setupjournal.Launchd {
	return envLaunchd{e.scheduler(), schedulerSite{userHome}}
}

type envLaunchd struct {
	scheduler scheduler
	site      schedulerSite
}

func (l envLaunchd) JobState(plist string) string {
	return l.scheduler.jobState(context.Background(), l.site, jobRef(plist))
}

func (l envLaunchd) Load(plist string) error {
	return l.scheduler.load(context.Background(), l.site, jobRef(plist))
}

func (l envLaunchd) Unload(plist string) error {
	return l.scheduler.unload(context.Background(), l.site, jobRef(plist))
}
