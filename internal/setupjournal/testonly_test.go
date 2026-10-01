package setupjournal

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// Launchd is a scheduler as these tests script it: jobs are named by the
// plist that defines them, and a state is one of the scheduler's JobState
// words. The journal drives schedulers by ref and site; backends adapts one of
// these to that, naming each job by its plist, so the tests say which plist
// they mean and never where its definition lives.
type Launchd interface {
	JobState(plist string) string
	Load(plist string) error
	Unload(plist string) error
}

// JobActive reports whether a state from Launchd.JobState is a loaded job.
func JobActive(state string) bool { return scheduler.JobState(state).Active() }

// JobAnotherInstallation is the state of a job the scheduler loaded from
// another installation's definition.
const JobAnotherInstallation = string(scheduler.AnotherInstallation)

// backends is l as the journal's Backends: every name resolves to it, launchd's.
func backends(l Launchd) Backends {
	return func(string) (scheduler.Scheduler, error) { return plistScheduler{l}, nil }
}

// plistScheduler is a scheduler.Scheduler over a Launchd. Its site is the
// plist itself (Locate refuses nothing) and its ref the plist's label, as
// launchd's is; every call acts on the plist the site names.
type plistScheduler struct{ l Launchd }

func (plistScheduler) Name() string { return "launchd" }

func (plistScheduler) Words() scheduler.Words {
	return scheduler.Words{Manager: "launchd", Job: "LaunchAgent", Definition: "plist", Tool: "launchctl", Name: "label"}
}

func (plistScheduler) Ref(scheduler.Installation) scheduler.Ref { panic("unused") }

func (plistScheduler) Plan(scheduler.Site, scheduler.Installation, scheduler.JobSpec) (scheduler.Plan, error) {
	panic("unused")
}

func (plistScheduler) DefaultPATH() string { return "" }

func (plistScheduler) Locate(definition string) (scheduler.Site, scheduler.Ref, error) {
	return scheduler.Site{UserHome: definition}, scheduler.Ref(simLabel(definition)), nil
}

func (p plistScheduler) Inspect(_ context.Context, site scheduler.Site, _ scheduler.Ref) scheduler.Status {
	return scheduler.Status{State: scheduler.JobState(p.l.JobState(site.UserHome))}
}

// Definition is a job with no definition on disk, which lists no paths.
func (plistScheduler) Definition(scheduler.Site, scheduler.Ref) scheduler.Status {
	return scheduler.Status{}
}

func (plistScheduler) Installed(context.Context, scheduler.Site, scheduler.Installation) ([]scheduler.Job, error) {
	panic("unused")
}

func (p plistScheduler) Load(_ context.Context, site scheduler.Site, _ scheduler.Ref) error {
	return p.l.Load(site.UserHome)
}

func (p plistScheduler) Unload(_ context.Context, site scheduler.Site, _ scheduler.Ref) error {
	return p.l.Unload(site.UserHome)
}

// fakeLaunchd stands in for launchd: each call goes to the matching func.
// With none set, a job is missing, and loading or stopping one fails.
type fakeLaunchd struct {
	state  func(plist string) string
	load   func(plist string) error
	unload func(plist string) error
}

func (f fakeLaunchd) JobState(plist string) string {
	if f.state == nil {
		return "missing"
	}
	return f.state(plist)
}

func (f fakeLaunchd) Load(plist string) error {
	if f.load == nil {
		return errors.New("fake launchd: unexpected load of " + plist)
	}
	return f.load(plist)
}

func (f fakeLaunchd) Unload(plist string) error {
	if f.unload == nil {
		return errors.New("fake launchd: unexpected unload of " + plist)
	}
	return f.unload(plist)
}

// noLock stands in for the collector lock Recover takes.
func noLock() (func(), error) { return func() {}, nil }

// launchdSim answers as launchd and cli's Launchd do, for tests of failure
// paths: loaded maps a label to the plist launchd loaded it from. JobState
// reports JobAnotherInstallation for a label loaded from another file;
// Unload stops a job only when launchd loaded it from that very plist, and
// refuses otherwise, as launchd.Scheduler.Unload does; Load of a label that
// is already loaded fails, as launchctl bootstrap does. failLoad and
// failUnload make the next n calls for a plist fail; unknown makes its
// state unknown.
type launchdSim struct {
	loaded     map[string]string
	failLoad   map[string]int
	failUnload map[string]int
	unknown    map[string]bool
	calls      []string
}

func newLaunchdSim() *launchdSim {
	return &launchdSim{loaded: map[string]string{}, failLoad: map[string]int{}, failUnload: map[string]int{}, unknown: map[string]bool{}}
}

func simLabel(plist string) string { return strings.TrimSuffix(filepath.Base(plist), ".plist") }

func (l *launchdSim) JobState(plist string) string {
	from, ok := l.loaded[simLabel(plist)]
	switch {
	case l.unknown[plist]:
		return "unknown"
	case !ok:
		return "missing"
	case from != plist:
		return JobAnotherInstallation
	}
	return "running"
}

func (l *launchdSim) Load(plist string) error {
	l.calls = append(l.calls, "load "+plist)
	if l.failLoad[plist] > 0 {
		l.failLoad[plist]--
		return errors.New("launchctl bootstrap: exit status 5: Bootstrap failed: 5: Input/output error")
	}
	if _, ok := l.loaded[simLabel(plist)]; ok {
		return errors.New("launchctl bootstrap: exit status 37: service already loaded")
	}
	l.loaded[simLabel(plist)] = plist
	return nil
}

func (l *launchdSim) Unload(plist string) error {
	l.calls = append(l.calls, "unload "+plist)
	state := l.JobState(plist)
	if state == "missing" {
		return nil
	}
	if state == JobAnotherInstallation {
		return errors.New("launchd's job was not loaded from " + plist + "; it belongs to another installation and was left running")
	}
	if !JobActive(state) {
		return errors.New("cannot confirm which plist launchd's job was loaded from; it was left as it is")
	}
	if l.failUnload[plist] > 0 {
		l.failUnload[plist]--
		return errors.New("launchctl bootout: exit status 5: Boot-out failed: 5: Input/output error")
	}
	delete(l.loaded, simLabel(plist))
	return nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
