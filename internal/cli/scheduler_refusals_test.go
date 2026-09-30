package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// The user-facing texts of the two job states setup must never act on:
// unknown (launchctl cannot say) and another_installation (launchd runs this
// label from another plist). Each call site words them differently, so each is
// a golden: setup's checks before its first question (preflight), the plan
// setup makes after the answers, uninstall, and the refusal to stop a job whose
// state changed between the check and the stop (unloadLaunchAgent, reached
// from setup's commit and from uninstall).
//
// The refusals go through the launchctl seam: r.answers scripts what launchctl
// print says for a label, one answer per call, the last one repeated.

// refusedStates are the two job states setup and uninstall refuse to act on.
var refusedStates = []launchdAnswer{answerUnknown, answerAnotherInstallation}

// checkRefusal compares what a command wrote, with the files it must leave,
// against testdata/scheduler/refusals/name.txt.
func (r *schedRun) checkRefusal(name string, code int, output string) {
	r.t.Helper()
	got := fmt.Sprintf("# exit %d\n%s", code, r.normalize(output))
	golden.Check(r.t, filepath.Join("testdata", "scheduler", "refusals", name+".txt"), []byte(got))
}

// Preflight: setup stops before its first question, for the interactive
// wizard and (naming the blocker again on standard error) for setup --yes.
func TestPreflightRefusalTexts(t *testing.T) {
	for _, state := range refusedStates {
		r := newSchedRun(t, true)
		r.answers[r.ownLabel()] = []launchdAnswer{state}
		code, out := r.run("setup")
		r.checkRefusal("preflight-"+string(state)+"-interactive", code, out)
		code, out = r.setup()
		r.checkRefusal("preflight-"+string(state)+"-yes", code, out)
		if r.journalPending() || len(r.loaded) != 0 {
			t.Fatalf("%s: preflight changed something", state)
		}
	}
}

// Plan: preflight saw nothing wrong, but launchctl's answer changed before
// setup planned its transaction. Nothing is written or loaded.
func TestSetupPlanRefusalTexts(t *testing.T) {
	for _, state := range refusedStates {
		r := newSchedRun(t, true)
		r.answers[r.ownLabel()] = []launchdAnswer{answerMissing, state}
		code, _ := r.setup()
		r.checkRefusal("setup-plan-"+string(state), code, r.stderr)
		if _, err := os.Stat(r.own()); !os.IsNotExist(err) || r.journalPending() {
			t.Fatalf("%s: setup wrote its plist or journal (%v)", state, err)
		}
	}
}

// Uninstall refuses on unknown before changing anything, and on
// another_installation leaves that job running and keeps this installation's
// plist (the "kept" path), while still removing everything else.
func TestUninstallRefusalTexts(t *testing.T) {
	for _, state := range refusedStates {
		r := newSchedRun(t, true)
		r.install()
		r.answers[r.ownLabel()] = []launchdAnswer{state}
		code, out := r.run("uninstall", "--yes")
		r.checkRefusal("uninstall-"+string(state), code, out)
		if _, err := os.Stat(r.own()); err != nil {
			t.Fatalf("%s: uninstall removed the plist: %v", state, err)
		}
		// Unknown stops uninstall before anything changes; another_installation
		// disables capture and removes the hooks, and only the plist stays.
		if hooksLeft := r.hooksInstalled(); hooksLeft == (state == answerAnotherInstallation) {
			t.Fatalf("%s: hooks left = %v", state, hooksLeft)
		}
		for _, call := range r.lines {
			if !strings.HasPrefix(call, "print ") {
				t.Fatalf("%s: uninstall asked launchctl to change something: %s", state, call)
			}
		}
	}
}

// Unload: the job was loaded from this installation's plist when checked, and
// launchctl says otherwise when setup's commit or uninstall goes to stop it. The
// refusal is wrapped by its caller: setup's commit rolls back, uninstall stops.
//
// When launchd runs the label from another plist, setup's rollback puts the
// files back and never asks launchctl to start this installation's job over
// the other's (launchd would refuse), so the record is removed and the error
// says the job belongs to another installation. The transcript pins that call
// sequence. When launchctl cannot say (unknown), the rollback stops and the
// record stays.
func TestUnloadRefusalTexts(t *testing.T) {
	for _, state := range refusedStates {
		r := newSchedRun(t, true)
		r.install()
		r.probe("own", r.own(), nil)
		r.answers[r.ownLabel()] = []launchdAnswer{answerLoaded, answerLoaded, state}
		code, _ := r.setup()
		r.checkRefusal("unload-in-setup-"+string(state), code, r.stderr)
		r.checkTranscript("unload-refused-in-setup-"+string(state), "setup whose stop of the job is refused: "+string(state)+" when the commit asks", code)

		r = newSchedRun(t, true)
		r.install()
		r.answers[r.ownLabel()] = []launchdAnswer{answerLoaded, state}
		code, out := r.run("uninstall", "--yes")
		r.checkRefusal("unload-in-uninstall-"+string(state), code, out)
	}
}
