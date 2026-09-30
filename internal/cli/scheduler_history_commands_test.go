package cli

import (
	"os"
	"strings"
	"testing"
)

// How status and uninstall treat the jobs an installation's history left
// (see scheduler.Inspector.Installed): status reports on the installation's
// own job, or, only when that has no plist, on the first collector an
// earlier release installed for it under another label; neither command ever
// asks about or touches the prototype's upload job, and uninstall keeps an
// earlier-label plist whose job launchd runs from another installation's plist.

// prints are the labels launchctl print was asked about since the last reset.
func (r *schedRun) prints() []string {
	var labels []string
	for _, line := range r.lines {
		call, _, _ := strings.Cut(line, "  |  ")
		if rest, ok := strings.CutPrefix(call, "print "+r.uid+"/"); ok {
			labels = append(labels, rest)
		}
	}
	return labels
}

func (r *schedRun) touched(label string) bool {
	for _, line := range r.lines {
		call, _, _ := strings.Cut(line, "  |  ")
		if strings.HasSuffix(call, "/"+label) || strings.HasSuffix(call, "/"+label+".plist") {
			return true
		}
	}
	return false
}

// Not parallel: it replaces launchctl.
func TestStatusReportsOnAnEarlierLabelOnlyWithoutItsOwnPlist(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	r.loadedPrototype()
	earlier := earlierLabel("/old/spelling/a")
	r.earlierCollector(earlier, true)

	status := func(want string) {
		t.Helper()
		r.lines = nil
		if code, out := r.run("status", "--json"); code != 0 {
			t.Fatalf("status: exit %d\n%s", code, out)
		}
		if got := r.prints(); len(got) != 1 || got[0] != want {
			t.Errorf("status asked launchctl about %q, want %s alone", got, want)
		}
		if r.touched(prototypeLabel) {
			t.Errorf("status asked about the prototype's job: %q", r.lines)
		}
	}
	// Its own plist is there: that job, although an earlier one is too.
	status(r.ownLabel())
	// No plist of its own: the earlier collector, never the prototype's job
	// although it comes first among the jobs the installation has.
	must(t, os.Remove(r.own()))
	delete(r.loaded, r.ownLabel())
	status(earlier)
	// Neither: its own job again.
	must(t, os.Remove(r.agents(earlier)))
	status(r.ownLabel())
}

// A prototype plist that is not the prototype's blocks setup, and only setup:
// status still falls back to an earlier collector, and uninstall still stops
// and removes it, leaving that plist as it is.
//
// Not parallel: it replaces launchctl.
func TestAnUnrecognizedPrototypeBlocksNeitherStatusNorUninstall(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	unrecognized := []byte(strings.Replace(prototypePlist, "skill_runs.py", "unrelated.py", 1))
	writeFile(t, r.agents(prototypeLabel), unrecognized)
	earlier := earlierLabel("/old/spelling/a")
	r.earlierCollector(earlier, true)
	must(t, os.Remove(r.own()))
	delete(r.loaded, r.ownLabel())

	r.lines = nil
	if code, out := r.run("status", "--json"); code != 0 {
		t.Fatalf("status: exit %d\n%s", code, out)
	}
	if got := r.prints(); len(got) != 1 || got[0] != earlier {
		t.Errorf("status asked launchctl about %q, want %s alone", got, earlier)
	}
	if code, out := r.run("uninstall", "--yes"); code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(r.agents(earlier)); !os.IsNotExist(err) || r.running(r.agents(earlier)) {
		t.Errorf("the earlier collector is left: %v, running %v", err, r.running(r.agents(earlier)))
	}
	if data, err := os.ReadFile(r.agents(prototypeLabel)); err != nil || string(data) != string(unrecognized) {
		t.Errorf("uninstall changed the unrecognized prototype plist: %v", err)
	}
}

// Not parallel: it replaces launchctl.
func TestUninstallLeavesThePrototypeAndAnotherInstallationsEarlierJobAlone(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	r.loadedPrototype()
	ours, theirs := earlierLabel("/old/spelling/a"), earlierLabel("/old/spelling/b")
	r.earlierCollector(ours, true)
	r.earlierCollector(theirs, false)
	r.answers[theirs] = []launchdAnswer{answerAnotherInstallation}
	code, out := r.run("uninstall", "--yes")
	if code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if r.touched(prototypeLabel) || !r.running(r.agents(prototypeLabel)) {
		t.Errorf("uninstall asked about or stopped the prototype's job: %q", r.lines)
	}
	if _, err := os.Stat(r.agents(prototypeLabel)); err != nil {
		t.Errorf("uninstall removed the prototype's plist: %v", err)
	}
	if _, err := os.Stat(r.agents(ours)); !os.IsNotExist(err) || r.running(r.agents(ours)) {
		t.Errorf("the earlier collector of this directory is left: %v, running %v", err, r.running(r.agents(ours)))
	}
	if _, err := os.Stat(r.agents(theirs)); err != nil {
		t.Errorf("uninstall removed the plist of a job another installation runs: %v", err)
	}
	if want := "Left launchd's " + theirs + " job running: it was loaded from another plist, so it belongs to another installation. " + r.agents(theirs) + " was kept."; !strings.Contains(out, want) {
		t.Errorf("uninstall output lacks %q:\n%s", want, out)
	}
	if _, err := os.Stat(r.own()); !os.IsNotExist(err) {
		t.Errorf("uninstall left its own plist: %v", err)
	}
}
