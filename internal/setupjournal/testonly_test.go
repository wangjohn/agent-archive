package setupjournal

import (
	"errors"
	"path/filepath"
	"strings"
)

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
// refuses otherwise, as cli's unloadLaunchAgent does; Load of a label that
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
