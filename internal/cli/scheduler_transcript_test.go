package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// What setup and uninstall ask launchctl, in what order, and what is on disk
// at each call (the harness is scheduler_harness_test.go). A behavior change
// is a golden diff. None of these tests is parallel: newSchedRun replaces
// launchctl.

// install runs an ordinary setup and forgets what launchctl was asked.
func (r *schedRun) install() {
	r.t.Helper()
	if code, out := r.setup(); code != 0 {
		r.t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.lines = nil
}

// checkTranscript compares the calls and the end state with the golden file
// testdata/scheduler/transcripts/name.txt.
func (r *schedRun) checkTranscript(name, title string, code int) {
	r.t.Helper()
	got := fmt.Sprintf("# %s\n# each launchctl call, then the state of the files as it was asked\n%s\n# exit %d, end: %s\n",
		title, strings.Join(r.lines, "\n"), code, r.state())
	golden.Check(r.t, filepath.Join("testdata", "scheduler", "transcripts", name+".txt"), []byte(r.normalize(got)))
}

// Every launchctl call setup makes, in order, for an installation with nothing
// installed: the collector's state is asked (setup's first check, then the
// plan), and only then the new job is loaded. The journal exists from before
// the first change until after the last.
func TestFirstSetupLaunchctlSequence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		defaultInstall bool
	}{{"first-setup-default", true}, {"first-setup-other-directory", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSchedRun(t, tc.defaultInstall)
			r.probe("own", r.own(), nil)
			code, out := r.setup()
			if code != 0 {
				t.Fatalf("setup: exit %d\n%s", code, out)
			}
			r.checkTranscript(tc.name, "first setup, nothing loaded", code)
		})
	}
}

// Setup over an installation whose job is loaded stops it first, writes the
// files (the plist changes from old to new), and starts the new job.
func TestResetupLaunchctlSequence(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	old, err := os.ReadFile(r.own())
	must(t, err)
	r.useNewExecutable()
	r.probe("own", r.own(), old)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	if data, _ := os.ReadFile(r.own()); bytes.Equal(data, old) {
		t.Fatal("the plist was not rewritten for the new executable")
	}
	r.checkTranscript("resetup-loaded", "setup again from another executable, job loaded", code)
}

// The collectors earlier releases left for this data directory are stopped
// and their plists removed after the new files are written and before the new
// job is loaded; one that launchd does not run is only removed.
func TestSetupRetiresEarlierLabelsBeforeLoading(t *testing.T) {
	r := newSchedRun(t, true)
	r.probe("own", r.own(), nil)
	r.earlierCollector(earlierLabel("/old/spelling/a"), true)
	r.earlierCollector(earlierLabel("/old/spelling/b"), false)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-retires-earlier-labels", "setup with two earlier-label collectors, one loaded", code)
}

// An installation in another data directory retires the collector that releases
// before labels were per directory installed under the default label.
func TestSetupMovesAJobOffTheDefaultLabelLaunchctlOrder(t *testing.T) {
	r := newSchedRun(t, false)
	r.probe("own", r.own(), nil)
	r.earlierCollector(defaultLabel, true)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-moves-job-off-default-label", "setup in another directory, its collector loaded under the default label", code)
}

// The default installation retires the prototype's upload job the same way.
func TestSetupMigratesThePrototypeJobBeforeLoading(t *testing.T) {
	r := newSchedRun(t, true)
	r.probe("own", r.own(), nil)
	r.loadedPrototype()
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-migrates-prototype", "setup with the prototype's upload job loaded", code)
}

// The whole history at once: this installation's job, the prototype's, and
// two earlier-label collectors all loaded. The order is the contract: stop own,
// write files, retire the prototype, retire the earlier labels, load own.
func TestSetupWithEveryKindOfOldJobLaunchctlOrder(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	old, err := os.ReadFile(r.own())
	must(t, err)
	r.useNewExecutable()
	r.probe("own", r.own(), old)
	r.loadedPrototype()
	r.earlierCollector(earlierLabel("/old/spelling/a"), true)
	r.earlierCollector(earlierLabel("/old/spelling/b"), true)
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-every-kind-of-old-job", "setup with own, prototype and two earlier-label jobs loaded", code)
}

// A new job that fails to start puts everything back: the old files, and the
// jobs setup stopped, restarted in the order they were stopped.
func TestFailedStartRollsBackLaunchctlSequence(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	old, err := os.ReadFile(r.own())
	must(t, err)
	r.useNewExecutable()
	r.probe("own", r.own(), old)
	r.loadedPrototype()
	r.earlierCollector(earlierLabel("/old/spelling/a"), true)
	r.failOne[r.own()] = true
	code, out := r.setup()
	if code == 0 {
		t.Fatalf("setup succeeded although the job could not start:\n%s", out)
	}
	r.checkTranscript("setup-start-fails-rollback", "setup whose new job fails to start", code)
}

// Uninstall stops this installation's job and every earlier-label collector
// of the directory that launchd runs, by service target, then removes their
// plists.
func TestUninstallLaunchctlSequence(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	r.probe("own", r.own(), nil)
	r.earlierCollector(earlierLabel("/old/spelling/a"), true)
	r.earlierCollector(earlierLabel("/old/spelling/b"), false)
	code, out := r.run("uninstall", "--yes")
	if code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	r.checkTranscript("uninstall-loaded", "uninstall with own and two earlier-label collectors, two loaded", code)
}

// The job stopped on its own between the check and the stop (launchd no longer
// has it when setup's commit or uninstall goes to stop it): nothing is booted
// out, and setup and uninstall carry on as if it had been stopped.
func TestJobGoneBeforeTheStopLaunchctlSequence(t *testing.T) {
	r := newSchedRun(t, true)
	r.install()
	r.probe("own", r.own(), nil)
	r.answers[r.ownLabel()] = []launchdAnswer{answerLoaded, answerLoaded, answerMissing}
	code, out := r.setup()
	if code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, out)
	}
	r.checkTranscript("setup-job-gone-before-the-stop", "setup whose loaded job is gone when the commit stops it", code)

	r = newSchedRun(t, true)
	r.install()
	r.probe("own", r.own(), nil)
	r.answers[r.ownLabel()] = []launchdAnswer{answerLoaded, answerMissing}
	code, out = r.run("uninstall", "--yes")
	if code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	r.checkTranscript("uninstall-job-gone-before-the-stop", "uninstall whose loaded job is gone when it stops it", code)
}
