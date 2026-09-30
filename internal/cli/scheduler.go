package cli

import (
	"context"
	"fmt"
	"path/filepath"

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

// jobState is the state of the job plist defines, for callers that hold the
// plist's path.
func (e Env) jobState(userHome, plist string) string {
	return string(e.scheduler().JobState(context.Background(), userSite(userHome), jobRef(plist)))
}

// unloadJob stops the job plist defines (see scheduler.Scheduler.Unload).
func (e Env) unloadJob(userHome, plist string) error {
	return e.scheduler().Unload(context.Background(), userSite(userHome), jobRef(plist))
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
	return string(l.scheduler.JobState(context.Background(), site, ref))
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
