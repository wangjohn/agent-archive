package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A journal written now records the scheduler that made each job and the job's
// ref, in fields a release without them ignores, and changes no field it has: a
// setup interrupted under this release is recovered by the release before it,
// and the fixtures (testdata/scheduler/journals, written by that release) are
// recovered by this one (TestInterruptedSetupJournalsReplay).
func TestInterruptedSetupJournalNamesItsBackendAndJobsAndChangesNothingElse(t *testing.T) {
	// Not parallel: newSchedRun replaces launchctl.
	for name, interrupt := range map[string]func(r *schedRun){
		// Both interrupt a setup without agent skills, whose files are not what
		// is read here; the journal's fields are the same.
		"first-setup-prototype": func(r *schedRun) {
			r.loadedPrototype()
			r.crashingSetup("--no-skills")
		},
		"resetup-earlier-labels": func(r *schedRun) {
			r.install()
			r.earlierCollector(earlierLabel("/old/spelling/a"), true)
			r.earlierCollector(earlierLabel("/old/spelling/b"), false)
			r.exe = r.useNewExecutable()
			r.crashingSetup("--no-skills")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSchedRun(t, true)
			interrupt(r)
			raw, err := os.ReadFile(r.journalPath())
			must(t, err)
			var got map[string]json.RawMessage
			must(t, json.Unmarshal(raw, &got))
			fixture, err := os.ReadFile(fixturePath(name))
			must(t, err)
			var old map[string]json.RawMessage
			must(t, json.Unmarshal(fixture, &old))

			// Every field the earlier release wrote is still written, and the
			// only new ones are the two.
			for key := range old {
				if _, ok := got[key]; !ok {
					t.Errorf("the journal lacks %q, which the release before wrote", key)
				}
			}
			for key := range got {
				if _, ok := old[key]; !ok && key != "backend" && key != "job_ref" {
					t.Errorf("the journal has the new field %q", key)
				}
			}
			var journal journalFile
			must(t, json.Unmarshal(raw, &journal))
			var backend, jobRef string
			must(t, json.Unmarshal(got["backend"], &backend))
			must(t, json.Unmarshal(got["job_ref"], &jobRef))
			if backend != "launchd" || jobRef != r.ownLabel() || plistLabel(journal.Plist) != jobRef {
				t.Errorf("backend %q, job_ref %q for the plist %s", backend, jobRef, journal.Plist)
			}
			// Each job it retires is named the same way, by the label its plist is
			// named after.
			jobs := 0
			for _, key := range []string{"legacy", "relabeled"} {
				jobs += checkRetiredJob(t, got[key])
			}
			var more []json.RawMessage
			if raw, ok := got["more_relabeled"]; ok {
				must(t, json.Unmarshal(raw, &more))
			}
			for _, job := range more {
				jobs += checkRetiredJob(t, job)
			}
			if want := len(journal.retired()); jobs != want || jobs == 0 {
				t.Errorf("%d retired jobs are named, want %d", jobs, want)
			}
		})
	}
}

// checkRetiredJob checks one retired job's record, when there is one, and
// says how many it checked.
func checkRetiredJob(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	if raw == nil {
		return 0
	}
	var job struct {
		Change  journalChange `json:"change"`
		Backend string        `json:"backend"`
		JobRef  string        `json:"job_ref"`
	}
	must(t, json.Unmarshal(raw, &job))
	if job.Backend != "launchd" || job.JobRef != strings.TrimSuffix(filepath.Base(job.Change.Path), ".plist") || job.JobRef == "" {
		t.Errorf("a retired job is recorded as %+v", job)
	}
	return 1
}

// The refresh's journal names the job the same way, so a refresh interrupted
// is recovered through the scheduler that made the collector.
func TestRefreshJournalNamesItsBackendAndJob(t *testing.T) {
	// Not parallel: newRefreshInstall replaces launchctl, which parallel
	// tests (TestIsolationFailsClosed) read.
	r := newRefreshInstall(t, false, true, modeLoaded)
	plan, err := planSetupRefresh(r.home, r.userHome, r.newExe, mustLoadConfig(t, r.home), r.env)
	must(t, err)
	if plan.journal.Backend != "launchd" || plan.journal.JobRef != r.label() || !slices.Contains([]string{r.plist}, plan.journal.Plist) {
		t.Errorf("refresh journal: backend %q, job_ref %q, plist %q; want launchd, %s, %s", plan.journal.Backend, plan.journal.JobRef, plan.journal.Plist, r.label(), r.plist)
	}
}

// A journal this system cannot drive (one a setup on another system's
// scheduler wrote, or whose job_ref names another job than its plist) blocks
// recovery before anything changes: no launchctl call, every file as the crash
// left it, the record kept, and the message says why and the way out.
func TestRecoveryOfAJournalThisSystemCannotDriveChangesNothing(t *testing.T) {
	// Not parallel: newSchedRun replaces launchctl.
	for name, tc := range map[string]struct {
		edit func(journal map[string]any)
		says string
	}{
		"another backend":                   {func(j map[string]any) { j["backend"] = "systemd" }, "the interrupted setup used the systemd scheduler, and this system's is launchd; its jobs were left as they are"},
		"a retired job of another backend":  {func(j map[string]any) { j["relabeled"].(map[string]any)["backend"] = "systemd" }, "used the systemd scheduler"},
		"a job_ref its plist does not name": {func(j map[string]any) { j["job_ref"] = "com.agent-archive.collector.0123456789ab" }, "the journal names the LaunchAgent com.agent-archive.collector.0123456789ab, but its plist"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSchedRun(t, true)
			journal := r.interrupted("resetup-earlier-labels", beforeStart)
			raw, err := os.ReadFile(r.journalPath())
			must(t, err)
			var edited map[string]any
			must(t, json.Unmarshal(raw, &edited))
			tc.edit(edited)
			raw, err = json.Marshal(edited)
			must(t, err)
			writeFile(t, r.journalPath(), raw)
			before := map[string][]byte{}
			for _, c := range journal.Changes {
				before[c.Path], _ = os.ReadFile(c.Path)
			}
			for _, job := range journal.retired() {
				before[job.Change.Path], _ = os.ReadFile(job.Change.Path)
			}
			code, out := r.run("setup")
			if code == 0 || !r.journalPending() || !strings.Contains(out, tc.says) || !strings.Contains(out, "so recovery changed nothing") || !strings.Contains(out, "setup --abandon-recovery") {
				t.Errorf("exit %d, record pending %v:\n%s", code, r.journalPending(), out)
			}
			if len(r.lines) != 0 {
				t.Errorf("launchctl was run: %q", r.lines)
			}
			for path, data := range before {
				if got, _ := os.ReadFile(path); !bytes.Equal(got, data) {
					t.Errorf("%s changed", path)
				}
			}
		})
	}
}
