package cli

import (
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// The tests name a job by where launchd keeps its plist, as the launchd
// adapter spells it; production code asks the scheduler (installation.ref,
// installation.installed and Status.Paths) and never builds such a path.

// label is the collector's job as text.
func (in installation) label() string { return string(in.ref()) }

// collectorPlist is the LaunchAgent plist of the background collector.
func (in installation) collectorPlist() string {
	return launchd.PlistPath(scheduler.Site{UserHome: in.userHome}, in.ref())
}

// previousCollectorPlists are the plists of the collectors earlier releases
// installed for this data directory under other labels, as the scheduler
// lists them.
func (in installation) previousCollectorPlists() []string {
	jobs, _ := in.installed(in.userHome)
	var plists []string
	for _, job := range jobs {
		if job.Alias == scheduler.EarlierLabel {
			plists = append(plists, launchd.PlistPath(scheduler.Site{UserHome: in.userHome}, job.Ref))
		}
	}
	return plists
}
