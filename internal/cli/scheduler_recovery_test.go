package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Recovery of a setup that was interrupted, replayed from journals the
// current code wrote (testdata/scheduler/journals; its README says how). The
// journal's on-disk format is read across releases, so these files must keep
// replaying unchanged: they are never regenerated, a new format gets new
// fixtures beside them.

// crashPoint is where in its commit an interrupted setup stopped.
type crashPoint string

const (
	// beforeChanges: the journal is written, nothing else happened. Every
	// file is as setup found it and every old job still runs.
	beforeChanges crashPoint = "before-changes"
	// beforeStart: every file changed and every old job stopped, the new job
	// not started yet.
	beforeStart crashPoint = "before-start"
	// afterStart: the new job started, the journal not yet removed.
	afterStart crashPoint = "after-start"
)

var crashPoints = []crashPoint{beforeChanges, beforeStart, afterStart}

// interrupted puts the fake Mac in the state a crash at crash left it in,
// with the journal fixture name as its record, and returns that record.
func (r *schedRun) interrupted(name string, crash crashPoint) journalFile {
	r.t.Helper()
	raw, err := os.ReadFile(fixturePath(name))
	must(r.t, err)
	raw = rewritePaths(r.t, raw, fixtureUserHome, r.userHome, fixtureAccount, r.account, fixtureProject, r.project, fixtureBin, r.exe, fixtureNewBin, r.exe+"-new")
	writeFile(r.t, r.journalPath(), raw)
	var journal journalFile
	must(r.t, json.Unmarshal(raw, &journal))
	if journal.Plist != r.own() {
		r.t.Fatalf("the fixture's collector plist %s is not this installation's %s", journal.Plist, r.own())
	}
	for _, c := range journal.Changes {
		switch {
		case crash != beforeChanges && c.Delete:
		case crash != beforeChanges:
			writeFile(r.t, c.Path, c.After)
		case c.Existed:
			writeFile(r.t, c.Path, c.Before)
		}
	}
	for _, job := range journal.retired() {
		if crash == beforeChanges {
			writeFile(r.t, job.Change.Path, job.Change.Before)
			if job.WasLoaded {
				r.loaded[plistLabel(job.Change.Path)] = job.Change.Path
			}
		}
		r.probe(plistLabel(job.Change.Path), job.Change.Path, nil)
	}
	if (crash == beforeChanges && journal.WasLoaded) || crash == afterStart {
		r.loaded[plistLabel(journal.Plist)] = journal.Plist
	}
	// The collector's plist reads "old" while it is as setup found it and
	// "new" while it is setup's, when setup found one.
	var found []byte
	for _, c := range journal.Changes {
		if c.Path == journal.Plist {
			found = c.Before
		}
	}
	r.probe("own", journal.Plist, found)
	return journal
}

// Replayed by the current code, an interrupted setup goes back to how it found
// everything: every file as it was, every job that was running running again,
// the journal gone. The launchctl calls and the files at each are the golden.
func TestInterruptedSetupJournalsReplay(t *testing.T) {
	for _, name := range []string{"resetup-earlier-labels", "first-setup-prototype"} {
		for _, crash := range crashPoints {
			t.Run(name+"-"+string(crash), func(t *testing.T) {
				r := newSchedRun(t, true)
				journal := r.interrupted(name, crash)
				// setup recovers first; then its wizard checks launchctl again
				// (the last print) and finds no answer to its first question,
				// so it exits 1 whether or not recovery succeeded.
				code, out := r.run("setup")
				if r.journalPending() {
					t.Fatalf("the journal remains: exit %d\n%s", code, out)
				}
				for _, c := range journal.Changes {
					data, err := os.ReadFile(c.Path)
					if found := err == nil; found != c.Existed || (found && !bytes.Equal(data, c.Before)) {
						t.Errorf("%s is not as setup found it (%v)", c.Path, err)
					}
				}
				for _, job := range journal.retired() {
					if data, err := os.ReadFile(job.Change.Path); err != nil || !bytes.Equal(data, job.Change.Before) {
						t.Errorf("%s was not put back (%v)", job.Change.Path, err)
					}
					if running := r.running(job.Change.Path); running != job.WasLoaded {
						t.Errorf("%s running = %v, was %v", job.Change.Path, running, job.WasLoaded)
					}
				}
				if running := r.running(journal.Plist); running != journal.WasLoaded {
					t.Errorf("the collector running = %v, was %v", running, journal.WasLoaded)
				}
				r.checkTranscript("replay-"+name+"-"+string(crash), name+" journal replayed by setup, crashed "+strings.ReplaceAll(string(crash), "-", " "), code)
			})
		}
	}
}

// setup --abandon-recovery discards the record and touches nothing else: it
// lists the files it kept, and leaves launchd alone.
func TestAbandonRecoveryKeepsEverythingAndListsIt(t *testing.T) {
	for _, name := range []string{"resetup-earlier-labels", "first-setup-prototype"} {
		r := newSchedRun(t, true)
		journal := r.interrupted(name, beforeStart)
		before := map[string][]byte{}
		for _, c := range journal.Changes {
			before[c.Path], _ = os.ReadFile(c.Path)
		}
		code, out := r.run("setup", "--abandon-recovery")
		r.checkRefusal("abandon-"+name, code, out)
		if r.journalPending() || len(r.lines) != 0 {
			t.Fatalf("%s: journal pending %v, launchctl calls %q", name, r.journalPending(), r.lines)
		}
		for path, data := range before {
			if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
				t.Errorf("%s: %s changed", name, path)
			}
		}
	}
}

// Every way recovery can stop, with the text the user sees and the way out it
// names. Each case starts from the same interrupted setups; the record stays.
func TestRecoveryBlockedTexts(t *testing.T) {
	const resetup, prototype = "resetup-earlier-labels", "first-setup-prototype"
	earlier := earlierLabel("/old/spelling/a")
	var got strings.Builder
	for _, tc := range []struct {
		name    string
		fixture string
		crash   crashPoint
		arrange func(r *schedRun, journal journalFile)
	}{
		{"a file setup changed was edited since", resetup, beforeStart, func(r *schedRun, j journalFile) {
			must(r.t, os.WriteFile(j.Changes[0].Path, []byte("edited by hand\n"), 0o600))
		}},
		{"an earlier collector's plist was edited since", resetup, beforeChanges, func(r *schedRun, j journalFile) {
			must(r.t, os.WriteFile(j.Relabeled.Change.Path, append(j.Relabeled.Change.Before, "<!-- edited -->"...), 0o600))
		}},
		{"the prototype's plist was edited since", prototype, beforeChanges, func(r *schedRun, j journalFile) {
			must(r.t, os.WriteFile(j.Legacy.Change.Path, append(j.Legacy.Change.Before, "<!-- edited -->"...), 0o600))
		}},
		{"launchctl cannot stop the collector", resetup, beforeChanges, func(r *schedRun, j journalFile) { r.failBootout[plistLabel(j.Plist)] = true }},
		{"launchctl cannot restart the collector", resetup, beforeStart, func(r *schedRun, j journalFile) { r.failOne[j.Plist] = true }},
		{"launchctl cannot restart an earlier collector", resetup, beforeStart, func(r *schedRun, j journalFile) { r.failOne[j.Relabeled.Change.Path] = true }},
		{"the collector's state is unknown", resetup, beforeChanges, func(r *schedRun, j journalFile) {
			r.answers[plistLabel(j.Plist)] = []launchdAnswer{answerUnknown}
		}},
		{"an earlier collector's state is unknown", resetup, beforeStart, func(r *schedRun, _ journalFile) { r.answers[earlier] = []launchdAnswer{answerUnknown} }},
		{"an earlier collector's label runs from another plist", resetup, beforeStart, func(r *schedRun, _ journalFile) {
			r.answers[earlier] = []launchdAnswer{answerAnotherInstallation}
		}},
		{"the prototype's state is unknown", prototype, beforeStart, func(r *schedRun, _ journalFile) {
			r.answers[prototypeLabel] = []launchdAnswer{answerUnknown}
		}},
		{"the record cannot be read", "", beforeChanges, func(r *schedRun, _ journalFile) {
			writeFile(r.t, r.journalPath(), []byte(`{"changes":[`))
		}},
	} {
		r := newSchedRun(t, true)
		var journal journalFile
		if tc.fixture != "" {
			journal = r.interrupted(tc.fixture, tc.crash)
		}
		tc.arrange(r, journal)
		code, out := r.run("setup")
		if !r.journalPending() {
			t.Errorf("%s: the record was removed", tc.name)
		}
		fmt.Fprintf(&got, "## %s (exit %d)\n%s\n", tc.name, code, r.normalize(out))
	}
	golden.Check(t, filepath.Join("testdata", "scheduler", "recovery", "blocked-texts.txt"), []byte(got.String()))
}

// Any other command tells the user about the record and both ways out.
func TestCommandsRefuseWhileSetupIsInterrupted(t *testing.T) {
	r := newSchedRun(t, true)
	r.interrupted("resetup-earlier-labels", beforeStart)
	var got strings.Builder
	for _, command := range []string{"uninstall --yes", "pause", "sync"} {
		code, out := r.run(strings.Fields(command)...)
		fmt.Fprintf(&got, "## agent-archive %s (exit %d)\n%s\n", command, code, r.normalize(out))
	}
	golden.Check(t, filepath.Join("testdata", "scheduler", "recovery", "commands-refuse.txt"), []byte(got.String()))
}
