package setupjournal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/testutil/schedulertest"
)

// The journal names each job by the backend that made it, so a setup that
// moved to another backend still retires and, on failure, restores the jobs the
// first one left, through that backend: here the collector is the model's, the
// prototype's job is launchd's, and an earlier label is the model's again.
type mixedFixture struct {
	home            string
	userHome        string
	model           *schedulertest.Model
	sim             *launchdSim
	site            scheduler.Site
	collector       string // the model's definition of the collector
	legacy          string // launchd's plist of the prototype's job
	earlier         string // the model's definition of an earlier job
	collectorBefore []byte
	journal         Journal
}

func newMixedFixture(t *testing.T) *mixedFixture {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	def := func(ref string) string { return filepath.Join(userHome, ".model", ref+".job") }
	legacy := filepath.Join(userHome, "Library", "LaunchAgents", "com.agent-skills.skill-runs-upload.plist")
	f := &mixedFixture{
		home: home, userHome: userHome, model: schedulertest.NewModel(), sim: newLaunchdSim(), site: scheduler.Site{UserHome: userHome},
		collector: def("model-collector"), earlier: def("model-earlier"), legacy: legacy,
		collectorBefore: []byte("old collector definition"),
		journal: Journal{
			Backend: "model", JobRef: "model-collector", Plist: def("model-collector"), WasLoaded: true,
			Changes:   []hooks.Change{{Path: def("model-collector"), Before: []byte("old collector definition"), After: []byte("new collector definition"), Existed: true, Mode: 0o600}},
			Legacy:    &LegacyJob{Backend: "launchd", JobRef: simLabel(legacy), Change: hooks.Change{Path: legacy, Before: []byte(legacyPlist), Existed: true, Mode: 0o600}, WasLoaded: true},
			Relabeled: &LegacyJob{Backend: "model", JobRef: "model-earlier", Change: hooks.Change{Path: def("model-earlier"), Before: []byte("earlier definition"), Existed: true, Mode: 0o600}, WasLoaded: true},
		},
	}
	for path, data := range map[string]string{f.collector: string(f.collectorBefore), f.earlier: "earlier definition", f.legacy: legacyPlist} {
		must(t, local.WriteBytes(path, []byte(data)))
	}
	f.model.Put(f.site, "model-collector", scheduler.Running)
	f.model.Put(f.site, "model-earlier", scheduler.Loaded)
	f.sim.loaded[simLabel(f.legacy)] = f.legacy
	return f
}

// backends resolves "model" and "launchd" to the two backends, and refuses any
// other name.
func (f *mixedFixture) backends(name string) (scheduler.Scheduler, error) {
	known := map[string]scheduler.Scheduler{"model": f.model, "launchd": plistScheduler{f.sim}}
	if sched, ok := known[name]; ok {
		return sched, nil
	}
	return nil, errors.New("no " + name + " scheduler here")
}

func TestCommitDrivesEachJobThroughTheBackendThatMadeIt(t *testing.T) {
	t.Parallel()
	f := newMixedFixture(t)
	if err := Commit(f.home, f.journal, Backends(f.backends)); err != nil {
		t.Fatal(err)
	}
	// The collector was stopped through the model at the site of its own
	// definition, then loaded again from the definition Commit applied; the
	// earlier label was retired through the model; the prototype through launchd.
	if got, want := f.model.Calls(), []string{"unload model-collector", "unload model-earlier", "load model-collector"}; !slices.Equal(got, want) {
		t.Errorf("the model was called %q, want %q", got, want)
	}
	if held := f.model.Held(f.site, "model-collector"); held != scheduler.Loaded {
		t.Errorf("the collector is %q after Commit", held)
	}
	if held := f.model.Held(f.site, "model-earlier"); held != scheduler.Missing {
		t.Errorf("the earlier job is %q, want it stopped", held)
	}
	for _, path := range []string{f.earlier, f.legacy} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s was not retired: %v", path, err)
		}
	}
	if data, _ := os.ReadFile(f.collector); string(data) != "new collector definition" {
		t.Errorf("the collector's definition is %q", data)
	}
	if got := f.sim.calls; len(got) != 1 || got[0] != "unload "+f.legacy {
		t.Errorf("launchd was asked %q; want only the prototype's stop", got)
	}
}

// When setup is interrupted after the stops and the file changes, before the
// collector starts, recovery puts every job back through its own backend: the
// model's collector and earlier job are loaded again, launchd's prototype job
// too, and every file is as it was found.
func TestRecoverRestartsEachJobThroughTheBackendThatMadeIt(t *testing.T) {
	t.Parallel()
	f := newMixedFixture(t)
	f.model.Put(f.site, "model-collector", scheduler.Missing)
	f.model.Put(f.site, "model-earlier", scheduler.Missing)
	delete(f.sim.loaded, simLabel(f.legacy))
	must(t, os.Remove(f.earlier))
	must(t, os.Remove(f.legacy))
	must(t, os.WriteFile(f.collector, []byte("new collector definition"), 0o600))
	must(t, local.Write(JournalPath(f.home), f.journal))
	if err := Recover(f.home, Backends(f.backends), noLock); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[scheduler.Ref]scheduler.JobState{"model-collector": scheduler.Loaded, "model-earlier": scheduler.Loaded} {
		if held := f.model.Held(f.site, ref); held != want {
			t.Errorf("%s is %q after recovery, want %q", ref, held, want)
		}
	}
	if f.sim.loaded[simLabel(f.legacy)] != f.legacy {
		t.Errorf("launchd's prototype job was not loaded again: %v", f.sim.loaded)
	}
	for path, want := range map[string]string{f.collector: string(f.collectorBefore), f.earlier: "earlier definition", f.legacy: legacyPlist} {
		if data, _ := os.ReadFile(path); string(data) != want {
			t.Errorf("%s reads %q after recovery, want %q", path, data, want)
		}
	}
	if TransactionPending(f.home) {
		t.Error("the journal remains")
	}
}

// A journal whose backend this build cannot drive changes nothing it cannot
// ask about: recovery is blocked, saying the scheduler's state is unknown, and
// a commit fails to stop the collector, leaving the record.
func TestAJobOfAnUnknownBackendIsNeverChanged(t *testing.T) {
	t.Parallel()
	f := newMixedFixture(t)
	f.journal.Backend = "systemd"
	err := Recover(f.home, Backends(f.backends), noLock)
	var blocked *RecoveryBlockedError
	if err != nil {
		t.Fatalf("recover without a journal: %v", err)
	}
	must(t, local.Write(JournalPath(f.home), f.journal))
	err = Recover(f.home, Backends(f.backends), noLock)
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "state is unknown") || !strings.Contains(err.Error(), "restore access to the scheduler") {
		t.Errorf("recovery of a systemd journal: %v", err)
	}
	if len(f.model.Calls()) != 0 || len(f.sim.calls) != 0 {
		t.Errorf("a job was asked about: %q %q", f.model.Calls(), f.sim.calls)
	}
	err = Commit(f.home, f.journal, Backends(f.backends))
	if err == nil || !strings.Contains(err.Error(), "stop previous collector: no systemd scheduler here") {
		t.Errorf("Commit = %v", err)
	}
	if len(f.model.Calls()) != 0 {
		t.Errorf("the model was asked %q", f.model.Calls())
	}
}

// A job is addressed at the site its own definition is in, whatever
// site the recovering setup runs at, and by the ref the journal records when it
// records one, or else the ref its definition names.
func TestJobsAreAddressedByTheirOwnSiteAndRecordedRef(t *testing.T) {
	t.Parallel()
	f := newMixedFixture(t)
	other := scheduler.Site{UserHome: t.TempDir()}
	// The same ref at another site: not this journal's job.
	f.model.Put(other, "model-collector", scheduler.Running)
	f.journal.JobRef = "" // absent: the ref is the one the definition's path names
	f.journal.Legacy, f.journal.Relabeled = nil, nil
	if err := Commit(f.home, f.journal, Backends(f.backends)); err != nil {
		t.Fatal(err)
	}
	if held := f.model.Held(other, "model-collector"); held != scheduler.Running {
		t.Errorf("a job of the same name at another site is %q; it must not be touched", held)
	}
	// A recorded ref wins over the one the path names.
	f = newMixedFixture(t)
	f.journal.Legacy, f.journal.Relabeled = nil, nil
	f.journal.JobRef = "recorded-ref"
	f.model.Put(f.site, "recorded-ref", scheduler.Running)
	must(t, local.WriteBytes(filepath.Join(f.userHome, ".model", "recorded-ref.job"), []byte("job=recorded-ref\n")))
	must(t, local.Write(JournalPath(f.home), f.journal))
	if err := Restore(f.home, f.journal, Backends(f.backends)); err != nil {
		t.Fatal(err)
	}
	if calls := f.model.Calls(); !slices.Contains(calls, "inspect recorded-ref") || slices.Contains(calls, "inspect model-collector") {
		t.Errorf("the model was asked %q; want the recorded ref", calls)
	}
}

// oldJournal is the journal as the releases before backend and job_ref read
// it: every field they have, and none they do not.
type oldJournal struct {
	Legacy        *oldJob           `json:"legacy"`
	Relabeled     *oldJob           `json:"relabeled"`
	MoreRelabeled []*oldJob         `json:"more_relabeled"`
	Changes       []json.RawMessage `json:"changes"`
	Plist         string            `json:"plist"`
	WasLoaded     bool              `json:"was_loaded"`
	FilesOnly     bool              `json:"files_only"`
}

type oldJob struct {
	Change    hooks.Change `json:"change"`
	WasLoaded bool         `json:"was_loaded"`
}

// The new fields are additive: a journal with them decodes, through the
// fields an older release knows, to exactly what it would have written; and a
// journal without them (every one written until now) is the collector's
// launchd job, by the plist the journal names.
func TestJournalBackendAndJobRefAreAdditive(t *testing.T) {
	t.Parallel()
	job := func(path string) *LegacyJob {
		return &LegacyJob{Change: hooks.Change{Path: path, Before: []byte("b"), Existed: true, Mode: 0o644}, WasLoaded: true}
	}
	old := Journal{Legacy: job("/l.plist"), Relabeled: job("/r.plist"), MoreRelabeled: []*LegacyJob{job("/m.plist")}, Changes: []hooks.Change{{Path: "/c", After: []byte("x"), Mode: 0o600}}, Plist: "/p.plist", WasLoaded: true, FilesOnly: true}
	next := old
	next.Backend, next.JobRef = "launchd", "com.agent-archive.collector"
	next.Legacy = &LegacyJob{Change: old.Legacy.Change, WasLoaded: true, Backend: "launchd", JobRef: "com.agent-skills.skill-runs-upload"}
	dir := t.TempDir()
	read := func(j Journal) (oldJournal, string) {
		t.Helper()
		path := filepath.Join(dir, "j.json")
		must(t, local.Write(path, j))
		raw, err := os.ReadFile(path)
		must(t, err)
		var got oldJournal
		must(t, local.Read(path, &got))
		return got, string(raw)
	}
	before, rawOld := read(old)
	after, rawNext := read(next)
	if strings.Contains(rawOld, "backend") || strings.Contains(rawOld, "job_ref") {
		t.Errorf("a journal with neither field wrote them:\n%s", rawOld)
	}
	for _, want := range []string{`"backend": "launchd"`, `"job_ref": "com.agent-archive.collector"`, `"job_ref": "com.agent-skills.skill-runs-upload"`} {
		if !strings.Contains(rawNext, want) {
			t.Errorf("the journal lacks %s:\n%s", want, rawNext)
		}
	}
	encode := func(v oldJournal) string { out, err := json.Marshal(v); must(t, err); return string(out) }
	if encode(before) != encode(after) {
		t.Errorf("an older release reads the new journal as\n%s\nnot\n%s", encode(after), encode(before))
	}
	// A journal with neither field names launchd and the job its plist names.
	var read2 Journal
	oldPath := filepath.Join(dir, "old.json")
	must(t, os.WriteFile(oldPath, []byte(`{"changes":[],"plist":"/home/me/Library/LaunchAgents/com.agent-archive.collector.plist","was_loaded":true}`), 0o600))
	must(t, local.Read(oldPath, &read2))
	names := map[string]scheduler.Ref{}
	resolve := func(name string) (scheduler.Scheduler, error) {
		return recordingLocator{plistScheduler{fakeLaunchd{}}, name, names}, nil
	}
	target := Backends(resolve).target(read2.Backend, read2.JobRef, read2.Plist)
	if target.err != nil || names[DefaultBackend] != "com.agent-archive.collector" || target.ref != "com.agent-archive.collector" {
		t.Errorf("an old journal's job: %+v, resolved %v", target, names)
	}
}

// recordingLocator notes which backend name each resolution asked for.
type recordingLocator struct {
	plistScheduler
	name  string
	names map[string]scheduler.Ref
}

func (r recordingLocator) Locate(definition string) (scheduler.Site, scheduler.Ref, error) {
	site, ref, err := r.plistScheduler.Locate(definition)
	r.names[r.name] = ref
	return site, ref, err
}
