package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// staleHooks makes Claude Code's hook file run another executable, so a
// refresh has hooks to repair without the running executable, and so the
// collector's plist, having changed.
func (r *refreshInstall) staleHooks(t *testing.T) {
	t.Helper()
	settings := filepath.Join(r.userHome, ".claude", "settings.json")
	must(t, os.WriteFile(settings, []byte(strings.ReplaceAll(readText(t, settings), r.oldExe, r.oldExe+"-old")), 0o600))
}

// The launchctl commands setup --refresh runs, as arguments and in order:
// nothing unless a plist it changes belongs to a job launchd asked about;
// a loaded job is stopped, then loaded again from the same plist; and a job
// that is not loaded is asked about once and left as it is.
func TestRefreshLaunchctlSequences(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    launchdMode
		upgrade bool
		arrange func(t *testing.T, r *refreshInstall)
		// want is the argv, in the words of a default target and plist.
		want    func(r *refreshInstall) []string
		exit    int
		message string
		after   launchdMode
	}{
		{name: "loaded job, plist changes", mode: modeLoaded, upgrade: true, after: modeLoaded,
			want: func(r *refreshInstall) []string {
				return []string{r.print(), r.print(), r.bootout(), r.bootstrap()}
			}},
		{name: "job not loaded, plist changes", mode: modeMissing, upgrade: true, after: modeMissing,
			want: func(r *refreshInstall) []string { return []string{r.print()} }},
		{name: "plist already runs the executable", mode: modeLoaded, after: modeLoaded,
			arrange: func(t *testing.T, r *refreshInstall) { t.Helper(); r.staleHooks(t) },
			want:    func(*refreshInstall) []string { return nil }},
		{name: "no plist", mode: modeLoaded, upgrade: true, after: modeLoaded,
			arrange: func(t *testing.T, r *refreshInstall) { t.Helper(); must(t, os.Remove(r.plist)) },
			want:    func(*refreshInstall) []string { return nil }},
		{name: "job state unreadable", mode: modeUnknown, upgrade: true, after: modeUnknown, exit: 1,
			message: "cannot determine the background job's state; restore access to launchctl and retry. Nothing was changed",
			want:    func(r *refreshInstall) []string { return []string{r.print()} }},
		{name: "another installation's job", mode: modeElsewhere, upgrade: true, after: modeElsewhere, exit: 1,
			message: "belongs to another installation; refresh leaves it running and changes nothing. Nothing was changed",
			want:    func(r *refreshInstall) []string { return []string{r.print()} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefreshInstall(t, false, tc.upgrade, tc.mode)
			if tc.arrange != nil {
				tc.arrange(t, r)
			}
			before := tree(t, r.home, r.userHome)
			code, stdout, stderr := r.run(t)
			if code != tc.exit || (tc.exit == 0 && stderr != "") || (tc.exit != 0 && (stdout != "" || !strings.Contains(stderr, tc.message))) {
				t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
			}
			if got, want := r.launchd.argv(), tc.want(r); !reflect.DeepEqual(got, want) {
				t.Errorf("launchctl calls\n%q\nwant\n%q", got, want)
			}
			if r.launchd.mode != tc.after {
				t.Errorf("the job is left %s, want %s", r.launchd.mode, tc.after)
			}
			if tc.exit != 0 {
				if changed := differences(before, tree(t, r.home, r.userHome)); len(changed) != 0 {
					t.Errorf("a refused refresh changed %v", changed)
				}
			}
		})
	}
}

// A refresh whose restart fails to load the new plist puts the old files
// back and loads the old plist again: stop, load (fails), then the rollback
// asks the job's state, and loads it.
func TestRefreshLaunchctlSequenceWhenTheRestartFails(t *testing.T) {
	r := newRefreshInstall(t, false, true, modeLoaded)
	r.launchd.failBootstrap = true
	before := tree(t, r.home, r.userHome)
	code, stdout, stderr := r.run(t)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "previous installation restored: start background collector: launchctl bootstrap: exit status 5: Bootstrap failed") {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	bootstrap := r.bootstrap()
	want := []string{r.print(), r.print(), r.bootout(), bootstrap, r.print(), bootstrap}
	if got := r.launchd.argv(); !reflect.DeepEqual(got, want) {
		t.Errorf("launchctl calls\n%q\nwant\n%q", got, want)
	}
	if r.launchd.mode != modeLoaded {
		t.Errorf("the job is left %s", r.launchd.mode)
	}
	if changed := differences(before, tree(t, r.home, r.userHome)); len(changed) != 0 {
		t.Errorf("a failed refresh left %v changed", changed)
	}
}

// With the job left as it is, neither the commit nor the rollback asks
// launchctl anything: a refresh whose plist is unchanged, and fails writing
// a file, is put back with no launchctl command at all, and one whose job is
// not loaded asks only what it asked to plan.
func TestRefreshFilesOnlyRollbackRunsNoLaunchctl(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into any directory")
	}
	for _, tc := range []struct {
		name    string
		mode    launchdMode
		upgrade bool
		want    func(r *refreshInstall) []string
	}{
		{"plist unchanged, job loaded", modeLoaded, false, func(*refreshInstall) []string { return nil }},
		{"plist changes, job not loaded", modeMissing, true, func(r *refreshInstall) []string { return []string{r.print()} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefreshInstall(t, false, tc.upgrade, tc.mode)
			if !tc.upgrade {
				r.staleHooks(t)
			}
			// The shared skill is written after the hook files and Claude Code's
			// skill, which are applied when this write fails.
			skillDir := filepath.Dir(agentsSkillPath(r.userHome))
			older := strings.Replace(readText(t, agentsSkillPath(r.userHome)), "Run exactly this command", "Run this command", 1)
			must(t, os.WriteFile(agentsSkillPath(r.userHome), []byte(older), 0o600))
			must(t, os.Chmod(skillDir, 0o500))
			t.Cleanup(func() { _ = os.Chmod(skillDir, 0o700) })
			before := tree(t, r.home, r.userHome)
			code, stdout, stderr := r.run(t)
			must(t, os.Chmod(skillDir, 0o700))
			if code != 1 || stdout != "" || !strings.Contains(stderr, "previous installation restored") {
				t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
			}
			if got, want := r.launchd.argv(), tc.want(r); !reflect.DeepEqual(got, want) {
				t.Errorf("launchctl calls\n%q\nwant\n%q", got, want)
			}
			if changed := differences(before, tree(t, r.home, r.userHome)); len(changed) != 0 {
				t.Errorf("a failed refresh left %v changed", changed)
			}
			if setupjournal.TransactionPending(r.home) {
				t.Error("the journal remains")
			}
		})
	}
}

// launchctl print gets two seconds, and a launchctl that changes a job gets
// thirty, from the context the command is run with: a hung launchctl ends on
// its own (the tests of the timeouts themselves are in
// scheduler_refresh_timeout_internal_test.go). Read from the context each
// call got, so it holds whatever runs launchctl.
func TestRefreshLaunchctlCallsAreBounded(t *testing.T) {
	r := newRefreshInstall(t, false, true, modeLoaded)
	if code, stdout, stderr := r.run(t); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, tc := range []struct {
		verb string
		min  time.Duration
		max  time.Duration
	}{
		{"print", time.Second, 2 * time.Second},
		{"bootout", 25 * time.Second, 30 * time.Second},
		{"bootstrap", 25 * time.Second, 30 * time.Second},
	} {
		left, ok := r.launchd.remaining[tc.verb]
		if !ok || left < tc.min || left > tc.max {
			t.Errorf("launchctl %s had %v before its deadline (seen: %v), want between %v and %v", tc.verb, left, ok, tc.min, tc.max)
		}
	}
}

// filesOnlyJournal is an interrupted refresh that left the job alone: a hook
// file and the collector's plist changed, and the journal never went away.
// changes are applied when apply is set, as a crash after writing them leaves
// the files.
func (r *refreshInstall) filesOnlyJournal(t *testing.T, apply bool) (settings, plist string, journal setupjournal.Journal) {
	t.Helper()
	settings = filepath.Join(r.userHome, ".claude", "settings.json")
	oldSettings, oldPlist := readText(t, settings), readText(t, r.plist)
	newPlist := []byte(strings.ReplaceAll(oldPlist, r.oldExe, r.newExe))
	journal = setupjournal.Journal{FilesOnly: true, Plist: r.plist, Changes: []hooks.Change{
		{Path: settings, Before: []byte(oldSettings), After: []byte(strings.ReplaceAll(oldSettings, r.oldExe, r.newExe)), Existed: true, Mode: 0o600},
		{Path: r.plist, Before: []byte(oldPlist), After: newPlist, Existed: true, Mode: 0o600},
	}}
	must(t, local.Write(setupjournal.JournalPath(r.home), journal))
	if apply {
		must(t, hooks.Apply(journal.Changes))
	}
	return settings, oldPlist, journal
}

// setup recovers an interrupted refresh's journal by putting the files back,
// and neither stops nor starts the job: the job was never touched. (Setup
// goes on to ask its questions and asks launchctl only what they need; it
// never changes a job.) A file edited since is not overwritten, and the
// journal is kept with the way out.
func TestSetupRecoversAnInterruptedFilesOnlyJournalWithoutTouchingTheJob(t *testing.T) {
	changing := func(calls []string) (out []string) {
		for _, call := range calls {
			if !strings.HasPrefix(call, "print ") {
				out = append(out, call)
			}
		}
		return out
	}
	t.Run("recovered", func(t *testing.T) {
		r := newRefreshInstall(t, false, true, modeLoaded)
		settings, oldPlist, _ := r.filesOnlyJournal(t, true)
		before := tree(t, r.home, r.userHome)
		oldSettings := strings.ReplaceAll(readText(t, settings), r.newExe, r.oldExe)
		var out, errOut strings.Builder
		env := r.env
		Run([]string{"setup"}, strings.NewReader(""), &out, &errOut, env)
		if setupjournal.TransactionPending(r.home) {
			t.Fatalf("the journal remains:\n%s\n%s", &out, &errOut)
		}
		if readText(t, settings) != oldSettings || readText(t, r.plist) != oldPlist {
			t.Errorf("the files were not put back")
		}
		if got := changing(r.launchd.argv()); len(got) != 0 {
			t.Errorf("recovery changed the job: %q", got)
		}
		if r.launchd.mode != modeLoaded {
			t.Errorf("the job is left %s", r.launchd.mode)
		}
		if len(differences(before, tree(t, r.home, r.userHome))) == 0 {
			t.Error("nothing was put back")
		}
	})
	t.Run("a file edited since", func(t *testing.T) {
		r := newRefreshInstall(t, false, true, modeLoaded)
		settings, _, _ := r.filesOnlyJournal(t, true)
		must(t, os.WriteFile(settings, []byte("{\"edited\":true}\n"), 0o600))
		before := tree(t, r.home, r.userHome)
		var out, errOut strings.Builder
		if code := Run([]string{"setup"}, strings.NewReader(""), &out, &errOut, r.env); code != 1 {
			t.Fatalf("exit %d\n%s\n%s", code, &out, &errOut)
		}
		for _, want := range []string{"cannot recover the interrupted setup: " + settings + " changed outside setup", "agent-archive setup --abandon-recovery"} {
			if !strings.Contains(out.String()+errOut.String(), want) {
				t.Errorf("output lacks %q:\n%s\n%s", want, &out, &errOut)
			}
		}
		if !setupjournal.TransactionPending(r.home) {
			t.Error("the journal is gone")
		}
		if calls := r.launchd.argv(); len(calls) != 0 {
			t.Errorf("launchctl: %q", calls)
		}
		if changed := differences(before, tree(t, r.home, r.userHome)); len(changed) != 0 {
			t.Errorf("recovery changed %v", changed)
		}
	})
	t.Run("abandoned", func(t *testing.T) {
		r := newRefreshInstall(t, false, true, modeLoaded)
		settings, _, journal := r.filesOnlyJournal(t, true)
		applied := readText(t, settings)
		var out, errOut strings.Builder
		if code := Run([]string{"setup", "--abandon-recovery"}, nil, &out, &errOut, r.env); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, &out, &errOut)
		}
		if !strings.Contains(out.String(), "Discarded the interrupted setup recorded in "+setupjournal.JournalPath(r.home)+". These files were kept as they are now:\n  "+settings+"\n  "+r.plist+"\n") {
			t.Errorf("output:\n%s", &out)
		}
		if setupjournal.TransactionPending(r.home) || readText(t, settings) != applied || readText(t, r.plist) != string(journal.Changes[1].After) {
			t.Error("abandoning changed a file or kept the journal")
		}
		if calls := r.launchd.argv(); len(calls) != 0 {
			t.Errorf("launchctl: %q", calls)
		}
	})
}
