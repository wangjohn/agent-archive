package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

// The journal's on-disk format is read across releases: a setup
// interrupted under one release is recovered by the next. It must not
// change with the Go names (Journal and LegacyJob were setupJournal and
// legacyJob in internal/cli), so it is pinned byte for byte here, including
// the relabeled/more_relabeled split.
func TestJournalFormatIsPinned(t *testing.T) {
	t.Parallel()
	job := func(path string, loaded bool) *LegacyJob {
		return &LegacyJob{Change: hooks.Change{Path: path, Before: []byte("b"), Existed: true, Mode: 0o644}, WasLoaded: loaded}
	}
	journal := Journal{
		Legacy:        job("/l.plist", true),
		Relabeled:     job("/r1.plist", true),
		MoreRelabeled: []*LegacyJob{job("/r2.plist", false)},
		Changes:       []hooks.Change{{Path: "/s.json", Before: []byte("x"), After: []byte("y"), Existed: true, Mode: 0o600}, {Path: "/gone", Before: []byte("z"), Existed: true, Mode: 0o600, Delete: true}},
		Plist:         "/p.plist",
		WasLoaded:     true,
	}
	// Written as setup writes it (local.Write indents it); compared compact.
	path := filepath.Join(t.TempDir(), "setup-transaction.json")
	if err := local.Write(path, journal); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := json.Compact(&got, written); err != nil {
		t.Fatal(err)
	}
	const want = `{"legacy":{"change":{"Path":"/l.plist","Before":"Yg==","After":null,"Existed":true,"Mode":420},"was_loaded":true},` +
		`"relabeled":{"change":{"Path":"/r1.plist","Before":"Yg==","After":null,"Existed":true,"Mode":420},"was_loaded":true},` +
		`"more_relabeled":[{"change":{"Path":"/r2.plist","Before":"Yg==","After":null,"Existed":true,"Mode":420},"was_loaded":false}],` +
		`"changes":[{"Path":"/s.json","Before":"eA==","After":"eQ==","Existed":true,"Mode":384},{"Path":"/gone","Before":"eg==","After":null,"Existed":true,"Mode":384,"Delete":true}],` +
		`"plist":"/p.plist","was_loaded":true}`
	if got.String() != want {
		t.Fatalf("journal format changed:\n got %s\nwant %s", got.String(), want)
	}
	// A release that retired at most one earlier-label job wrote only
	// "relabeled"; it still reads, and an empty journal omits every job.
	if err := os.WriteFile(path, []byte(`{"relabeled":{"change":{"Path":"/r1.plist"},"was_loaded":true},"changes":[],"plist":"/p.plist","was_loaded":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var old Journal
	if err := local.Read(path, &old); err != nil {
		t.Fatal(err)
	}
	if jobs := old.relabeled(); len(jobs) != 1 || jobs[0].Change.Path != "/r1.plist" || !jobs[0].WasLoaded {
		t.Fatalf("old journal's relabeled job: %+v", jobs)
	}
	if order := journal.relabeled(); len(order) != 2 || order[0].Change.Path != "/r1.plist" || order[1].Change.Path != "/r2.plist" {
		t.Fatalf("relabeled order: %+v", order)
	}
	if err := local.Write(path, Journal{}); err != nil {
		t.Fatal(err)
	}
	if empty, _ := os.ReadFile(path); string(empty) != "{\n  \"changes\": null,\n  \"plist\": \"\",\n  \"was_loaded\": false\n}\n" {
		t.Fatalf("empty journal: %s", empty)
	}
}

// txFixture is an installation mid-upgrade: this directory's collector,
// loaded from an older plist; the prototype's upload job, loaded; two
// collectors under earlier labels (one loaded, one not) and one whose
// label launchd runs from another installation's plist; and a hook file,
// a new config file and the collector's plist the setup changes.
type txFixture struct {
	home      string
	own       string // this directory's collector plist
	settings  string
	config    string
	legacy    string
	other     string // an earlier label another installation runs
	relabeled []string
	sim       *launchdSim
	journal   Journal
	files     map[string][]byte // as found; nil when absent
	loaded    map[string]string
}

func newTxFixture(t *testing.T) *txFixture {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	agents := filepath.Join(userHome, "Library", "LaunchAgents")
	f := &txFixture{
		home:     home,
		own:      filepath.Join(agents, "com.agent-archive.collector.plist"),
		settings: filepath.Join(userHome, ".claude", "settings.json"),
		config:   filepath.Join(home, "config.json"),
		legacy:   filepath.Join(agents, legacyLaunchLabel+".plist"),
		relabeled: []string{
			filepath.Join(agents, "com.agent-archive.collector.aaaa.plist"),
			filepath.Join(agents, "com.agent-archive.collector.bbbb.plist"),
		},
		other: filepath.Join(agents, "com.agent-archive.collector.cccc.plist"),
		sim:   newLaunchdSim(),
	}
	write := func(path, data string) {
		t.Helper()
		if err := local.WriteBytes(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	write(f.own, "old collector plist")
	write(f.settings, "{\"theme\": \"light\"}\n")
	write(f.legacy, legacyPlist)
	for _, p := range append(slices.Clone(f.relabeled), f.other) {
		write(p, "earlier collector "+filepath.Base(p))
	}
	f.sim.loaded[simLabel(f.own)] = f.own
	f.sim.loaded[simLabel(f.legacy)] = f.legacy
	f.sim.loaded[simLabel(f.relabeled[0])] = f.relabeled[0]
	f.sim.loaded[simLabel(f.other)] = "/Users/real/Library/LaunchAgents/" + filepath.Base(f.other)

	// The jobs the scheduler lists as this installation's aliases (another
	// installation's job under an earlier label is not among them: it is not
	// this setup's to retire), as found.
	retiree := func(path string, alias scheduler.Alias) scheduler.Retiree {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return scheduler.Retiree{Backend: "launchd", Ref: scheduler.Ref(simLabel(path)), Alias: alias, Artifacts: []scheduler.Artifact{scheduler.FileArtifact(path, data, 0o644)}, WasLoaded: JobActive(f.sim.JobState(path))}
	}
	legacy, relabeled, err := RetireeJobs([]scheduler.Retiree{
		retiree(f.legacy, scheduler.Prototype),
		retiree(f.relabeled[0], scheduler.EarlierLabel),
		retiree(f.relabeled[1], scheduler.EarlierLabel),
	})
	if err != nil || legacy == nil || !legacy.WasLoaded || len(relabeled) != 2 || relabeled[0].Change.Path != f.relabeled[0] || !relabeled[0].WasLoaded || relabeled[1].Change.Path != f.relabeled[1] || relabeled[1].WasLoaded {
		t.Fatalf("retirees: %+v %+v %v", legacy, relabeled, err)
	}
	f.journal = Journal{
		Legacy:        legacy,
		Relabeled:     relabeled[0],
		MoreRelabeled: relabeled[1:],
		Changes: []hooks.Change{
			{Path: f.own, Before: []byte("old collector plist"), After: []byte("new collector plist"), Existed: true, Mode: 0o644},
			{Path: f.settings, Before: []byte("{\"theme\": \"light\"}\n"), After: []byte("{\"theme\": \"light\", \"hooks\": {}}\n"), Existed: true, Mode: 0o600},
			{Path: f.config, After: []byte("new config"), Mode: 0o600},
		},
		Plist:     f.own,
		WasLoaded: JobActive(f.sim.JobState(f.own)),
	}
	f.files = f.snapshot()
	f.loaded = maps.Clone(f.sim.loaded)
	f.sim.calls = nil
	return f
}

func (f *txFixture) snapshot() map[string][]byte {
	files := map[string][]byte{}
	for _, p := range append([]string{f.own, f.settings, f.config, f.legacy, f.other}, f.relabeled...) {
		data, err := os.ReadFile(p)
		if err == nil {
			files[p] = data
		} else {
			files[p] = nil
		}
	}
	return files
}

// requireAsFound fails unless every file and job is as the fixture found
// them and no journal remains.
func (f *txFixture) requireAsFound(t *testing.T) {
	t.Helper()
	for p, want := range f.files {
		got := f.snapshot()[p]
		if !bytes.Equal(got, want) || (got == nil) != (want == nil) {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
	if !maps.Equal(f.sim.loaded, f.loaded) {
		t.Errorf("launchd: %v, want %v", f.sim.loaded, f.loaded)
	}
	if TransactionPending(f.home) {
		t.Error("the journal remains")
	}
}

// crash leaves the state of a setup that stopped right after writing its
// journal (applied false: the collector still runs), or after stopping the
// collector and applying its file changes, before it retired or started
// anything.
func (f *txFixture) crash(t *testing.T, applied bool) {
	t.Helper()
	if err := local.Write(JournalPath(f.home), f.journal); err != nil {
		t.Fatal(err)
	}
	if applied {
		if err := f.sim.Unload(f.own); err != nil {
			t.Fatal(err)
		}
		if err := hooks.Apply(f.journal.Changes); err != nil {
			t.Fatal(err)
		}
	}
	f.sim.calls = nil
}

func TestCommitRetiresEveryOwnedJobAndNothingElse(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t)
	if err := Commit(f.home, f.journal, backends(f.sim)); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.journal.Changes {
		if got, _ := os.ReadFile(c.Path); !bytes.Equal(got, c.After) {
			t.Errorf("%s: %q", c.Path, got)
		}
	}
	for _, p := range append([]string{f.legacy}, f.relabeled...) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not retired (%v)", p, err)
		}
	}
	want := map[string]string{simLabel(f.own): f.own, simLabel(f.other): f.loaded[simLabel(f.other)]}
	if !maps.Equal(f.sim.loaded, want) {
		t.Errorf("launchd: %v, want %v", f.sim.loaded, want)
	}
	if got, _ := os.ReadFile(f.other); string(got) != "earlier collector "+filepath.Base(f.other) {
		t.Errorf("another installation's plist changed: %q", got)
	}
	if TransactionPending(f.home) {
		t.Error("the journal remains")
	}
}

// A launchctl failure at any step of a commit puts every file and job back
// as it was, and says which step failed.
func TestCommitRollsBackAFailureAtEachStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want string
		fail func(f *txFixture)
	}{
		{"stop the collector", "stop previous collector", func(f *txFixture) { f.sim.failUnload[f.own] = 1 }},
		{"retire the prototype's job", "Boot-out failed", func(f *txFixture) { f.sim.failUnload[f.legacy] = 1 }},
		{"retire an earlier label", "retire the " + relabeledJobName, func(f *txFixture) { f.sim.failUnload[f.relabeled[0]] = 1 }},
		{"start the collector", "start background collector", func(f *txFixture) { f.sim.failLoad[f.own] = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTxFixture(t)
			tc.fail(f)
			err := Commit(f.home, f.journal, backends(f.sim))
			if err == nil || !strings.Contains(err.Error(), "previous installation restored") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
			f.requireAsFound(t)
			for _, call := range f.sim.calls {
				if strings.HasSuffix(call, f.other) {
					t.Errorf("touched another installation's job: %s", call)
				}
			}
		})
	}
}

// Commit's stop of the previous collector is refused because another
// installation runs the label by then: the rollback puts the files back
// without starting this installation's job over the other's, so it completes
// (no journal left, no launchctl blame) and the error names the other
// installation.
func TestCommitRollbackLeavesAnotherInstallationsCollectorAlone(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t)
	elsewhere := "/Users/real/Library/LaunchAgents/" + filepath.Base(f.own)
	f.sim.loaded[simLabel(f.own)] = elsewhere
	f.loaded[simLabel(f.own)] = elsewhere
	err := Commit(f.home, f.journal, backends(f.sim))
	var blocked *RecoveryBlockedError
	if err == nil || errors.As(err, &blocked) || !strings.Contains(err.Error(), "previous installation restored") || !strings.Contains(err.Error(), "belongs to another installation") || strings.Contains(err.Error(), "launchctl") {
		t.Fatalf("err = %v", err)
	}
	f.requireAsFound(t)
	for _, call := range f.sim.calls {
		if call != "unload "+f.own {
			t.Errorf("asked launchd for more than the refused stop: %s", call)
		}
	}
}

// A rollback that launchd stops halfway keeps the journal and says how to
// get out; recovery once launchd works puts everything back.
func TestCommitReportsAnIncompleteRollback(t *testing.T) {
	t.Parallel()
	f := newTxFixture(t)
	f.sim.failLoad[f.own] = 1
	f.sim.failLoad[f.legacy] = 1
	err := Commit(f.home, f.journal, backends(f.sim))
	var blocked *RecoveryBlockedError
	if err == nil || !strings.Contains(err.Error(), "start background collector") || !strings.Contains(err.Error(), "rollback incomplete; run setup again") || !errors.As(err, &blocked) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(blocked.Guidance(), JournalPath(f.home)) || !strings.Contains(blocked.Guidance(), "--abandon-recovery") {
		t.Fatalf("guidance: %s", blocked.Guidance())
	}
	if !TransactionPending(f.home) {
		t.Fatal("an incomplete rollback removed its journal")
	}
	if err := Recover(f.home, backends(f.sim), noLock); err != nil {
		t.Fatal(err)
	}
	f.requireAsFound(t)
}

// Recovery checks everything it would put back before it asks launchd to
// do anything, and stops, naming the way out, when it cannot go on.
func TestRestoreRefusesBeforeTouchingAnything(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want string
		edit func(t *testing.T, f *txFixture)
	}{
		{"hook file edited", "settings.json changed outside setup", func(t *testing.T, f *txFixture) {
			t.Helper()
			if err := os.WriteFile(f.settings, []byte(`{"theme": "dark"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"prototype plist edited", "legacy upload job's plist", func(t *testing.T, f *txFixture) {
			t.Helper()
			if err := os.WriteFile(f.legacy, []byte(legacyPlist+"<!-- edited -->"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"earlier-label plist edited", relabeledJobName + "'s plist", func(t *testing.T, f *txFixture) {
			t.Helper()
			if err := os.WriteFile(f.relabeled[1], []byte("edited"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"collector state unknown", "state is unknown", func(t *testing.T, f *txFixture) { t.Helper(); f.sim.unknown[f.own] = true }},
	} {
		for _, applied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/applied=%v", tc.name, applied), func(t *testing.T) {
				t.Parallel()
				f := newTxFixture(t)
				f.crash(t, applied)
				tc.edit(t, f)
				before, loaded := f.snapshot(), maps.Clone(f.sim.loaded)
				err := Recover(f.home, backends(f.sim), noLock)
				var blocked *RecoveryBlockedError
				if !errors.As(err, &blocked) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v", err)
				}
				if len(f.sim.calls) != 0 {
					t.Errorf("asked launchd to %v before refusing", f.sim.calls)
				}
				if after := f.snapshot(); !maps.EqualFunc(after, before, bytes.Equal) || !maps.Equal(f.sim.loaded, loaded) {
					t.Error("a refused recovery changed something")
				}
				if !TransactionPending(f.home) {
					t.Error("a refused recovery removed its journal")
				}
			})
		}
	}
}

// A label another installation now runs is never stopped or replaced by
// recovery, whether it is the collector's own or a retired job's: the files
// and plists go back, every other job is put back as it was, the job is left
// to the other installation, and the journal is removed, since nothing more
// can be done for it (a bootstrap over it fails, and used to leave the
// journal stuck behind a message that blamed launchctl, or, for a retired
// job, one that stopped recovery until --abandon-recovery).
func TestRecoveryLeavesAnotherInstallationsJobAlone(t *testing.T) {
	t.Parallel()
	t.Run("the collector's label", func(t *testing.T) {
		t.Parallel()
		f := newTxFixture(t)
		f.crash(t, true)
		elsewhere := "/Users/real/Library/LaunchAgents/" + filepath.Base(f.own)
		f.sim.loaded[simLabel(f.own)] = elsewhere
		if err := Recover(f.home, backends(f.sim), noLock); err != nil {
			t.Fatal(err)
		}
		f.loaded[simLabel(f.own)] = elsewhere
		f.requireAsFound(t)
		for _, call := range f.sim.calls {
			if strings.HasSuffix(call, f.own) {
				t.Errorf("asked launchd to %s, though another installation runs that label", call)
			}
		}
	})
	for name, retired := range map[string]func(f *txFixture) string{
		"the prototype's label": func(f *txFixture) string { return f.legacy },
		"an earlier label":      func(f *txFixture) string { return f.relabeled[0] },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newTxFixture(t)
			f.crash(t, true)
			plist := retired(f)
			// Setup had retired every job before it stopped (it stops the
			// collector first; the rest of its plan is done by hand here),
			// so the jobs restored after this one are restarted or put back
			// too; since then, another installation loaded this one's
			// label from its own plist.
			for _, job := range append([]*LegacyJob{f.journal.Legacy}, f.journal.relabeled()...) {
				if err := retireLegacyJob(job, backends(f.sim)); err != nil {
					t.Fatal(err)
				}
			}
			elsewhere := "/Users/real/Library/LaunchAgents/" + filepath.Base(plist)
			f.sim.loaded[simLabel(plist)] = elsewhere
			f.sim.calls = nil
			if err := Recover(f.home, backends(f.sim), noLock); err != nil {
				t.Fatal(err)
			}
			f.loaded[simLabel(plist)] = elsewhere
			f.requireAsFound(t)
			for _, call := range f.sim.calls {
				if strings.HasSuffix(call, plist) {
					t.Errorf("asked launchd to %s, though another installation runs that label", call)
				}
			}
		})
	}
}

// Recover reads the journal before it takes the collector lock: with no
// journal, or one it cannot read, it takes no lock and changes nothing.
func TestRecoverTakesTheLockOnlyForAReadableJournal(t *testing.T) {
	t.Parallel()
	locked := func(taken *bool) func() (func(), error) {
		return func() (func(), error) { *taken = true; return func() {}, nil }
	}
	home := t.TempDir()
	sim := newLaunchdSim()
	var taken bool
	if err := Recover(home, backends(sim), locked(&taken)); err != nil || taken {
		t.Fatalf("no journal: err=%v lock taken=%v", err, taken)
	}
	if err := os.WriteFile(JournalPath(home), []byte(`{"changes":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Recover(home, backends(sim), locked(&taken))
	var blocked *RecoveryBlockedError
	if !errors.As(err, &blocked) || !strings.Contains(err.Error(), "could not be read") || taken || len(sim.calls) != 0 {
		t.Fatalf("corrupt journal: err=%v lock taken=%v calls=%v", err, taken, sim.calls)
	}
	if got, _ := os.ReadFile(JournalPath(home)); string(got) != `{"changes":[` {
		t.Fatalf("the unreadable journal changed: %q", got)
	}
	if err := local.Write(JournalPath(home), Journal{Plist: "/p.plist", WasLoaded: true}); err != nil {
		t.Fatal(err)
	}
	busy := errors.New("a collector pass is running")
	if err := Recover(home, backends(sim), func() (func(), error) { return nil, busy }); !errors.Is(err, busy) || len(sim.calls) != 0 || !TransactionPending(home) {
		t.Fatalf("lock held: err=%v calls=%v", err, sim.calls)
	}
}

// A files-only journal (setup --refresh leaving the job as it is) commits,
// rolls back, and recovers without launchd being asked anything, even for a
// loaded job: it neither starts nor stops it.
func TestFilesOnlyTransactionNeverAsksLaunchd(t *testing.T) {
	t.Parallel()
	// The fake fails every load and unload, and records every question, and
	// every backend a job is resolved through: a files-only journal names no
	// job to drive, so it never resolves one, whatever backend it records.
	newQuiet := func() (backends Backends, asked func() []string) {
		var questions []string
		launchd := fakeLaunchd{state: func(plist string) string { questions = append(questions, plist); return "running" }}
		return func(name string) (scheduler.Scheduler, error) {
			questions = append(questions, "the "+name+" backend")
			return plistScheduler{launchd}, nil
		}, func() []string { return questions }
	}
	newJournal := func(t *testing.T) (home string, journal Journal, settings, added string) {
		t.Helper()
		home, dir := t.TempDir(), t.TempDir()
		settings, added = filepath.Join(dir, "settings.json"), filepath.Join(dir, "sub", "SKILL.md")
		if err := os.WriteFile(settings, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		journal = Journal{FilesOnly: true, Plist: filepath.Join(dir, "job.plist"), Changes: []hooks.Change{
			{Path: settings, Before: []byte("old"), After: []byte("new"), Existed: true, Mode: 0o600},
			{Path: added, After: []byte("skill"), Mode: 0o600},
		}}
		return home, journal, settings, added
	}

	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		quiet, asked := newQuiet()
		defer func() {
			if got := asked(); len(got) != 0 {
				t.Errorf("launchd was asked about %v", got)
			}
		}()
		home, journal, settings, added := newJournal(t)
		if err := Commit(home, journal, quiet); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(settings); string(got) != "new" {
			t.Errorf("settings: %q", got)
		}
		if got, _ := os.ReadFile(added); string(got) != "skill" {
			t.Errorf("added: %q", got)
		}
		if TransactionPending(home) {
			t.Error("the journal remains")
		}
	})
	t.Run("rollback of a failed write", func(t *testing.T) {
		t.Parallel()
		quiet, asked := newQuiet()
		defer func() {
			if got := asked(); len(got) != 0 {
				t.Errorf("launchd was asked about %v", got)
			}
		}()
		home, journal, settings, added := newJournal(t)
		// The second write fails: its directory cannot be written to.
		if err := os.Mkdir(filepath.Dir(added), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(added), 0o700) })
		err := Commit(home, journal, quiet)
		if err == nil || !strings.Contains(err.Error(), "previous installation restored") {
			t.Fatalf("err = %v", err)
		}
		if got, _ := os.ReadFile(settings); string(got) != "old" {
			t.Errorf("settings after rollback: %q", got)
		}
		if TransactionPending(home) {
			t.Error("the journal remains")
		}
	})
	t.Run("recovery of an interrupted transaction", func(t *testing.T) {
		t.Parallel()
		quiet, asked := newQuiet()
		defer func() {
			if got := asked(); len(got) != 0 {
				t.Errorf("launchd was asked about %v", got)
			}
		}()
		home, journal, settings, added := newJournal(t)
		if err := local.Write(JournalPath(home), journal); err != nil {
			t.Fatal(err)
		}
		if err := hooks.Apply(journal.Changes); err != nil {
			t.Fatal(err)
		}
		if err := Recover(home, quiet, noLock); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(settings); string(got) != "old" {
			t.Errorf("settings after recovery: %q", got)
		}
		if _, err := os.Stat(added); !os.IsNotExist(err) {
			t.Errorf("the added file remains: %v", err)
		}
		if TransactionPending(home) {
			t.Error("the journal remains")
		}
	})
}

// files_only is written only when set, so the ordinary transaction's
// journal keeps its format.
func TestFilesOnlyIsOmittedFromAnOrdinaryJournal(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "setup-transaction.json")
	written := func(journal Journal) string {
		t.Helper()
		if err := local.Write(path, journal); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := written(Journal{}); strings.Contains(got, "files_only") {
		t.Fatalf("an ordinary journal names files_only: %s", got)
	}
	if got := written(Journal{FilesOnly: true}); !strings.Contains(got, `"files_only": true`) {
		t.Fatalf("a files-only journal does not: %s", got)
	}
}
