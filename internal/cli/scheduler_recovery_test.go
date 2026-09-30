package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Recovery of a setup that was interrupted, replayed from journals the
// current code wrote (testdata/scheduler/journals; its README says how). The
// journal's on-disk format is read across releases, so these files must keep
// replaying unchanged: they are never regenerated, a new format gets new
// fixtures beside them.

// The journal fixtures name the paths of the fake Mac they were written on;
// a replay maps them to its own temporary folders.
const (
	fixtureUserHome = "/fixture/user-home"
	fixtureAccount  = "/fixture/account"
	fixtureProject  = "/fixture/project"
	fixtureBin      = "/fixture/bin/agent-archive"
	fixtureNewBin   = "/fixture/bin-new/agent-archive"
)

// rewritePaths rewrites the paths in a journal by pairs (old, new, ...).
func rewritePaths(t *testing.T, raw []byte, pairs ...string) []byte {
	t.Helper()
	return transformJournal(t, raw, strings.NewReplacer(pairs...).Replace)
}

// The journal fields that hold a path, and the ones that hold a file's bytes.
var (
	journalPathKeys = map[string]bool{"Path": true, "plist": true}
	journalFileKeys = map[string]bool{"Before": true, "After": true}
)

// transformJournal returns the journal in raw with every path field, and every
// file's recorded bytes (Before and After, which are base64), passed through f.
// Everything else, including fields this code does not know, is kept.
func transformJournal(t *testing.T, raw []byte, f func(text string) string) []byte {
	t.Helper()
	var tree any
	must(t, json.Unmarshal(raw, &tree))
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, value := range v {
				text, isText := value.(string)
				switch {
				case isText && journalPathKeys[key]:
					v[key] = f(text)
				case isText && journalFileKeys[key]:
					data, err := base64.StdEncoding.DecodeString(text)
					must(t, err)
					v[key] = base64.StdEncoding.EncodeToString([]byte(f(string(data))))
				default:
					walk(value)
				}
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(tree)
	out, err := json.MarshalIndent(tree, "", "  ")
	must(t, err)
	return append(out, '\n')
}

func fixturePath(name string) string {
	return filepath.Join("testdata", "scheduler", "journals", name+".json")
}

// interrupted puts the fake Mac in the state a crash left it in, with the
// journal fixture name as its record: applied (setup crashed just before
// starting the new job: every file changed, every old job stopped) or not
// (crashed before the first change: every file as found, every old job still
// running).
func (r *schedRun) interrupted(name string, applied bool) setupjournal.Journal {
	r.t.Helper()
	raw, err := os.ReadFile(fixturePath(name))
	must(r.t, err)
	raw = rewritePaths(r.t, raw, fixtureUserHome, r.userHome, fixtureAccount, r.account, fixtureProject, r.project, fixtureBin, r.exe, fixtureNewBin, r.exe+"-new")
	must(r.t, local.WriteBytes(setupjournal.JournalPath(r.home), raw))
	var journal setupjournal.Journal
	must(r.t, local.Read(setupjournal.JournalPath(r.home), &journal))
	if journal.Plist != r.own() {
		r.t.Fatalf("the fixture's collector plist %s is not this installation's %s", journal.Plist, r.own())
	}
	for _, c := range journal.Changes {
		switch {
		case applied && c.Delete:
		case applied:
			must(r.t, local.WriteBytes(c.Path, c.After))
		case c.Existed:
			must(r.t, local.WriteBytes(c.Path, c.Before))
		}
	}
	for _, job := range append([]*setupjournal.LegacyJob{journal.Legacy, journal.Relabeled}, journal.MoreRelabeled...) {
		if job == nil {
			continue
		}
		if !applied {
			must(r.t, local.WriteBytes(job.Change.Path, job.Change.Before))
			if job.WasLoaded {
				r.fake.loaded[launchLabel(job.Change.Path)] = job.Change.Path
			}
		}
		r.probe(launchLabel(job.Change.Path), job.Change.Path, nil)
	}
	if !applied && journal.WasLoaded {
		r.fake.loaded[launchLabel(journal.Plist)] = journal.Plist
	}
	r.probe("own", journal.Plist, nil)
	return journal
}

// Replayed by the current code, an interrupted setup goes back to how it found
// everything: every file as it was, every job that was running running again,
// the journal gone. The launchctl calls and the files at each are the golden.
func TestInterruptedSetupJournalsReplay(t *testing.T) {
	for _, name := range []string{"resetup-earlier-labels", "first-setup-prototype"} {
		for _, applied := range []bool{false, true} {
			state := map[bool]string{false: "before-changes", true: "before-start"}[applied]
			t.Run(name+"-"+state, func(t *testing.T) {
				r := newSchedRun(t, true)
				journal := r.interrupted(name, applied)
				code, out := r.run("setup")
				if setupjournal.TransactionPending(r.home) {
					t.Fatalf("the journal remains: exit %d\n%s", code, out)
				}
				for _, c := range journal.Changes {
					if !c.Unapplied() {
						t.Errorf("%s is not as setup found it", c.Path)
					}
				}
				jobs := append([]*setupjournal.LegacyJob{journal.Legacy, journal.Relabeled}, journal.MoreRelabeled...)
				for _, job := range jobs {
					if job == nil {
						continue
					}
					if data, err := os.ReadFile(job.Change.Path); err != nil || !bytes.Equal(data, job.Change.Before) {
						t.Errorf("%s was not put back (%v)", job.Change.Path, err)
					}
					if _, running := r.fake.loaded[launchLabel(job.Change.Path)]; running != job.WasLoaded {
						t.Errorf("%s running = %v, was %v", job.Change.Path, running, job.WasLoaded)
					}
				}
				if _, running := r.fake.loaded[launchLabel(journal.Plist)]; running != journal.WasLoaded {
					t.Errorf("the collector running = %v, was %v", running, journal.WasLoaded)
				}
				r.checkTranscript("replay-"+name+"-"+state, name+" journal replayed by setup, crashed "+strings.ReplaceAll(state, "-", " "), code)
			})
		}
	}
}

// setup --abandon-recovery discards the record and touches nothing else: it
// lists the files it kept, and leaves launchd alone.
func TestAbandonRecoveryKeepsEverythingAndListsIt(t *testing.T) {
	for _, name := range []string{"resetup-earlier-labels", "first-setup-prototype"} {
		r := newSchedRun(t, true)
		journal := r.interrupted(name, true)
		before := map[string][]byte{}
		for _, c := range journal.Changes {
			before[c.Path], _ = os.ReadFile(c.Path)
		}
		code, out := r.run("setup", "--abandon-recovery")
		r.checkRefusal("abandon-"+name, code, out)
		if setupjournal.TransactionPending(r.home) || len(r.lines) != 0 {
			t.Fatalf("%s: journal pending %v, launchctl calls %q", name, setupjournal.TransactionPending(r.home), r.lines)
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
	earlier := hooks.CollectorLabel("/old/spelling/a", "")
	var got strings.Builder
	for _, tc := range []struct {
		name    string
		fixture string
		applied bool
		arrange func(r *schedRun, journal setupjournal.Journal)
	}{
		{"a file setup changed was edited since", resetup, true, func(r *schedRun, j setupjournal.Journal) {
			must(r.t, os.WriteFile(j.Changes[0].Path, []byte("edited by hand\n"), 0o600))
		}},
		{"an earlier collector's plist was edited since", resetup, false, func(r *schedRun, j setupjournal.Journal) {
			must(r.t, os.WriteFile(j.Relabeled.Change.Path, append(j.Relabeled.Change.Before, "<!-- edited -->"...), 0o600))
		}},
		{"the prototype's plist was edited since", prototype, false, func(r *schedRun, j setupjournal.Journal) {
			must(r.t, os.WriteFile(j.Legacy.Change.Path, append(j.Legacy.Change.Before, "<!-- edited -->"...), 0o600))
		}},
		{"launchctl cannot stop the collector", resetup, false, func(r *schedRun, j setupjournal.Journal) { r.failBootout[launchLabel(j.Plist)] = true }},
		{"launchctl cannot restart the collector", resetup, true, func(r *schedRun, j setupjournal.Journal) { r.failOne[j.Plist] = true }},
		{"launchctl cannot restart an earlier collector", resetup, true, func(r *schedRun, j setupjournal.Journal) { r.failOne[j.Relabeled.Change.Path] = true }},
		{"the collector's state is unknown", resetup, false, func(r *schedRun, j setupjournal.Journal) {
			r.answers[launchLabel(j.Plist)] = []launchdAnswer{answerUnknown}
		}},
		{"an earlier collector's state is unknown", resetup, true, func(r *schedRun, j setupjournal.Journal) { r.answers[earlier] = []launchdAnswer{answerUnknown} }},
		{"an earlier collector's label runs from another plist", resetup, true, func(r *schedRun, j setupjournal.Journal) {
			r.answers[earlier] = []launchdAnswer{answerAnotherInstallation}
		}},
		{"the prototype's state is unknown", prototype, true, func(r *schedRun, j setupjournal.Journal) {
			r.answers[setupjournal.LegacyLaunchLabel] = []launchdAnswer{answerUnknown}
		}},
		{"the record cannot be read", "", false, func(r *schedRun, _ setupjournal.Journal) {
			must(r.t, local.WriteBytes(setupjournal.JournalPath(r.home), []byte(`{"changes":[`)))
		}},
	} {
		r := newSchedRun(t, true)
		var journal setupjournal.Journal
		if tc.fixture != "" {
			journal = r.interrupted(tc.fixture, tc.applied)
		}
		tc.arrange(r, journal)
		code, out := r.run("setup")
		if !setupjournal.TransactionPending(r.home) {
			t.Errorf("%s: the record was removed", tc.name)
		}
		fmt.Fprintf(&got, "## %s (exit %d)\n%s\n", tc.name, code, r.normalize(out))
	}
	golden.Check(t, filepath.Join("testdata", "scheduler", "recovery", "blocked-texts.txt"), []byte(got.String()))
}

// Any other command tells the user about the record and both ways out.
func TestCommandsRefuseWhileSetupIsInterrupted(t *testing.T) {
	r := newSchedRun(t, true)
	r.interrupted("resetup-earlier-labels", true)
	var got strings.Builder
	for _, command := range []string{"uninstall --yes", "pause", "sync"} {
		code, out := r.run(strings.Fields(command)...)
		fmt.Fprintf(&got, "## agent-archive %s (exit %d)\n%s\n", command, code, r.normalize(out))
	}
	golden.Check(t, filepath.Join("testdata", "scheduler", "recovery", "commands-refuse.txt"), []byte(got.String()))
}
