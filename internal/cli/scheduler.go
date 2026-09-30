package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// newScheduler is the scheduler a nil Env.Scheduler means: this system's own
// (host.Default), made when a command needs it, and making it runs nothing.
// It is a variable so that tests fail closed: isolateProcessForTesting
// replaces it with one whose launchctl stops the test, so a test that builds a
// bare Env{} can never reach the developer's real launchd (or, on Linux, its
// systemd). A test that means to drive launchd's own code stubs launchctl with
// stubLaunchctl.
var newScheduler = host.Default

// jobRef is the job the plist at path defines.
func jobRef(plist string) scheduler.Ref { return scheduler.Ref(launchd.Label(plist)) }

// collectorJob is the background collector as a scheduler defines it: the
// executable running `_collect` every minute, and once at load, for the data
// directory home, with the environment its storage needs (which never holds
// a credential: see collectorEnvironment).
func collectorJob(executable, home string, environment map[string]string) scheduler.JobSpec {
	return scheduler.JobSpec{Executable: executable, Args: []string{"_collect"}, DataHome: home, Env: environment, Interval: time.Minute, RunAtLoad: true}
}

// planJob is the plan that defines spec as this installation's job at the
// site of userHome. It reads nothing.
func (in installation) planJob(userHome string, spec scheduler.JobSpec) (scheduler.Plan, error) {
	return in.definer().Plan(userSite(userHome), in.schedulerInstallation(), spec)
}

// artifactChanges are the changes a plan makes to disk, as the setup journal
// records them: each artifact's file as it is now, and what it becomes. The
// port's artifacts are files alone.
func artifactChanges(artifacts []scheduler.Artifact) ([]hooks.Change, error) {
	if len(artifacts) == 0 {
		return nil, errors.New("the scheduler defined the job with nothing to write")
	}
	changes := make([]hooks.Change, 0, len(artifacts))
	for _, artifact := range artifacts {
		path, ok := artifact.Path()
		if !ok {
			return nil, fmt.Errorf("cannot write %s: only files are defined", artifact.ID)
		}
		before, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		changes = append(changes, hooks.Change{Path: path, Before: before, After: artifact.After, Existed: err == nil, Mode: artifact.Mode})
	}
	return changes, nil
}

// problemOf is the facts a status gives for a state that blocks a command,
// empty when the scheduler gave none.
func problemOf(status scheduler.Status) scheduler.Problem {
	if status.Problem == nil {
		return scheduler.Problem{}
	}
	return *status.Problem
}

// jobActive is whether a job is loaded, whether or not it is running now.
func jobActive(state scheduler.JobState) bool { return setupjournal.JobActive(string(state)) }

// scheduler is e's job scheduler: Env.Scheduler, or this system's own.
func (e Env) scheduler() scheduler.Scheduler {
	if e.Scheduler != nil {
		return e.Scheduler
	}
	return newScheduler()
}

// userSite is the site of the user home userHome, spelled as plistJob spells
// the site of a plist in it (cleaned: $HOME may end in a separator), so a
// setup that plans with one and commits with the other names its job at one
// site.
func userSite(userHome string) scheduler.Site {
	return scheduler.Site{UserHome: filepath.Clean(userHome)}
}

// defaultPATH is the PATH the scheduler gives a job whose definition sets
// none.
func (e Env) defaultPATH() string { return e.scheduler().DefaultPATH() }

// jobStatus is what the scheduler and the disk say about the job ref names
// (see scheduler.Inspector.Inspect).
func (e Env) jobStatus(userHome string, ref scheduler.Ref) scheduler.Status {
	return e.scheduler().Inspect(context.Background(), userSite(userHome), ref)
}

// unloadJob stops the job ref names (see scheduler.Controller.Unload).
func (e Env) unloadJob(userHome string, ref scheduler.Ref) error {
	return e.scheduler().Unload(context.Background(), userSite(userHome), ref)
}

// jobDefinition is what the definition of the job ref names says, without
// asking the scheduler about the job (see scheduler.Inspector.Definition).
func (e Env) jobDefinition(userHome string, ref scheduler.Ref) scheduler.Status {
	return e.scheduler().Definition(userSite(userHome), ref)
}

// launchd is e's scheduler as internal/setupjournal drives it: the same one
// every command uses, so a test's stand-in (and TestMain's failing launchctl)
// applies there too.
//
// setupjournal names each job by the plist it recorded, and a journal is
// recovered by whichever setup runs next for its data directory, perhaps with
// another $HOME (a sandbox overrides $HOME, and so the LaunchAgents directory,
// but not launchd). So a job is addressed at the site its own plist is in,
// never at the current user home: recovery stops and starts exactly the plist
// it restores, as it always has, and never one of the same label elsewhere.
func (e Env) launchd() setupjournal.Launchd { return envLaunchd{e.scheduler()} }

type envLaunchd struct{ scheduler scheduler.Scheduler }

// plistJob is the site and ref of the job plist defines: plist is
// <user home>/Library/LaunchAgents/<label>.plist, as every plist setup
// records is (each release has built them with filepath.Join, which this
// gives back whatever $HOME's spelling). Any other path names no job the
// scheduler can be asked about, and is refused rather than taken for another
// plist.
func plistJob(plist string) (scheduler.Site, scheduler.Ref, error) {
	site, ref := scheduler.Site{UserHome: filepath.Dir(filepath.Dir(filepath.Dir(plist)))}, jobRef(plist)
	if launchd.PlistPath(site, ref) != plist {
		return site, ref, fmt.Errorf("%s is not a LaunchAgent plist (<home>/Library/LaunchAgents/<label>.plist); launchd was left as it is", plist)
	}
	return site, ref, nil
}

// JobState is "unknown" for a plist plistJob refuses, so setupjournal
// changes nothing for it.
func (l envLaunchd) JobState(plist string) string {
	site, ref, err := plistJob(plist)
	if err != nil {
		return string(scheduler.Unknown)
	}
	return string(l.scheduler.Inspect(context.Background(), site, ref).State)
}

func (l envLaunchd) Load(plist string) error {
	site, ref, err := plistJob(plist)
	if err != nil {
		return err
	}
	return l.scheduler.Load(context.Background(), site, ref)
}

func (l envLaunchd) Unload(plist string) error {
	site, ref, err := plistJob(plist)
	if err != nil {
		return err
	}
	return l.scheduler.Unload(context.Background(), site, ref)
}
