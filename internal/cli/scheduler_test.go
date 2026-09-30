package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// launchdScheduler finds a job's definition from its ref and the user home
// alone, so every plist a command names (this installation's own, an earlier
// release's under another label, the prototype's upload job) must be the one
// its ref and site give back.
func TestLaunchdSchedulerFindsEveryPlistFromRefAndSite(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	in := env.installation(home, userHome)
	oldLabel := launchd.CollectorLabel(home+"-earlier", "")
	earlier := filepath.Join(userHome, "Library", "LaunchAgents", oldLabel+".plist")
	data, err := launchd.LaunchAgent("/opt/old/agent-archive", home, oldLabel, nil)
	must(t, err)
	must(t, local.WriteBytes(earlier, data))
	if got := in.previousCollectorPlists(); !slices.Equal(got, []string{earlier}) {
		t.Fatalf("previousCollectorPlists = %q, want %q", got, earlier)
	}
	site := schedulerSite{userHome}
	prototype := filepath.Join(userHome, "Library", "LaunchAgents", setupjournal.LegacyLaunchLabel+".plist")
	for _, plist := range []string{in.collectorPlist(), earlier, prototype} {
		if got := site.launchAgent(jobRef(plist)); got != plist {
			t.Errorf("the job of %s is found at %s", plist, got)
		}
	}
}

// Recovery addresses the job the journal names, at the plist it recorded,
// whatever $HOME the setup that recovers it runs with: a setup interrupted in
// a sandbox that overrides $HOME (and so its LaunchAgents, but not launchd)
// is recovered by one without it, and recovering it must stop and start that
// sandbox's plist, never one of the same label in the real home (another
// installation's job, or none).
func TestRecoveryAddressesTheJournalsPlistNotTheCurrentHomes(t *testing.T) {
	home, sandbox, account := t.TempDir(), t.TempDir(), t.TempDir()
	label := launchd.CollectorLabel(home, "")
	plist := filepath.Join(sandbox, "Library", "LaunchAgents", label+".plist")
	must(t, local.WriteBytes(plist, []byte("new")))
	must(t, local.WriteBytes(filepath.Join(account, "Library", "LaunchAgents", label+".plist"), []byte("real")))
	journal := setupjournal.Journal{
		Changes:   []hooks.Change{{Path: plist, Before: []byte("old"), After: []byte("new"), Existed: true, Mode: 0o644}},
		Plist:     plist,
		WasLoaded: true,
	}
	must(t, local.Write(setupjournal.JournalPath(home), journal))
	var argv []string
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		argv = append(argv, strings.Join(args, " "))
		if args[0] == "print" {
			return []byte("path = " + plist + "\nstate = running\n"), nil
		}
		return nil, nil
	})

	must(t, recoverSetup(home, Env{UserHomeDir: func() (string, error) { return account, nil }}))

	target := serviceTarget(plist)
	want := []string{"print " + target, "print " + target, "bootout " + target, "bootstrap " + strings.TrimSuffix(target, "/"+label) + " " + plist}
	if !slices.Equal(argv, want) {
		t.Errorf("launchctl calls\n%q\nwant\n%q", argv, want)
	}
	if got := readText(t, plist); got != "old" {
		t.Errorf("the journal's plist reads %q after recovery", got)
	}
}

// setupjournal names jobs by plist path; envLaunchd reads each job's ref off
// the path and asks the scheduler about it at the home the plist is in, which
// is not always the current user's (see
// TestRecoveryAddressesTheJournalsPlistNotTheCurrentHomes).
func TestEnvLaunchdNamesJobsByRefAtThePlistsHome(t *testing.T) {
	t.Parallel()
	sched := &siteRecorder{fakeScheduler: newFakeScheduler(t, "loaded")}
	launchd := Env{Scheduler: sched}.launchd()
	sandbox, account := filepath.Join("sandbox", "me"), "/Users/me"
	plist := filepath.Join(sandbox, "Library", "LaunchAgents", "com.agent-archive.collector.plist")
	other := filepath.Join(account, "Library", "LaunchAgents", "com.agent-archive.collector.0123456789ab.plist")
	if got := launchd.JobState(plist); got != "loaded" {
		t.Errorf("JobState = %q", got)
	}
	must(t, launchd.Unload(plist))
	if got := launchd.JobState(plist); got != "missing" {
		t.Errorf("JobState after Unload = %q", got)
	}
	must(t, launchd.Load(plist))
	must(t, launchd.Unload(other))
	want := []string{"state com.agent-archive.collector", "unload com.agent-archive.collector", "state com.agent-archive.collector", "load com.agent-archive.collector", "unload com.agent-archive.collector.0123456789ab"}
	if got := sched.all(); !slices.Equal(got, want) {
		t.Errorf("scheduler calls %q, want %q", got, want)
	}
	if want := []string{sandbox, sandbox, sandbox, sandbox, account}; !slices.Equal(sched.sites, want) {
		t.Errorf("scheduler sites %q, want %q", sched.sites, want)
	}
	if got := sched.state("com.agent-archive.collector"); got != "loaded" {
		t.Errorf("state after Load = %q", got)
	}
}

// A plist that is not <home>/Library/LaunchAgents/<label>.plist names no job
// a ref and site give back, so envLaunchd asks the scheduler nothing about it
// rather than address another plist: its state is unknown, which
// setupjournal changes nothing for, and loading or stopping it fails.
func TestEnvLaunchdRefusesAPlistNoRefAndSiteName(t *testing.T) {
	t.Parallel()
	sched := newFakeScheduler(t, "loaded")
	launchd := Env{Scheduler: sched}.launchd()
	for _, plist := range []string{
		"/synthetic/job",
		"/Users/me/Library/LaunchDaemons/com.agent-archive.collector.plist",
		"/Users/me/Library/LaunchAgents/com.agent-archive.collector.PLIST",
		"/Users/me/Library/LaunchAgents/../LaunchAgents/com.agent-archive.collector.plist",
	} {
		if got := launchd.JobState(plist); got != "unknown" {
			t.Errorf("JobState(%s) = %q, want unknown", plist, got)
		}
		if err := launchd.Load(plist); err == nil || !strings.Contains(err.Error(), plist) {
			t.Errorf("Load(%s) = %v", plist, err)
		}
		if err := launchd.Unload(plist); err == nil || !strings.Contains(err.Error(), plist) {
			t.Errorf("Unload(%s) = %v", plist, err)
		}
	}
	if got := sched.all(); len(got) != 0 {
		t.Errorf("the scheduler was asked %q", got)
	}
}

// A setup plans with the user home (Env.jobState) and commits and recovers
// with the plist its journal records (envLaunchd), so both must name every
// plist a release journals as the same job at the same site, however $HOME
// is spelled; the refusal above never reaches one of them.
func TestEveryJournaledPlistIsOneJobAtOneSite(t *testing.T) {
	t.Parallel()
	for _, userHome := range []string{"/Users/me", "/Users/me/", "/Users/me//", "/Users/./me/../me", "/", "me", "./me/", "."} {
		sched := &siteRecorder{fakeScheduler: newFakeScheduler(t, "loaded")}
		env := Env{Scheduler: sched, AccountHome: func() (string, error) { return "/Users/account", nil }}
		in := env.installation("/Users/me/archive", userHome)
		earlier := filepath.Join(userHome, "Library", "LaunchAgents", launchd.CollectorLabel("/Users/me/Archive", "")+".plist")
		prototype := filepath.Join(userHome, "Library", "LaunchAgents", setupjournal.LegacyLaunchLabel+".plist")
		for _, plist := range []string{in.collectorPlist(), earlier, prototype} {
			sched.sites = nil
			sched.forget()
			if got := env.jobState(userHome, plist); got != "loaded" {
				t.Errorf("$HOME %q: jobState(%s) = %q", userHome, plist, got)
			}
			if got := env.launchd().JobState(plist); got != "loaded" {
				t.Errorf("$HOME %q: the journal's JobState(%s) = %q", userHome, plist, got)
			}
			must(t, env.unloadJob(userHome, plist))
			must(t, env.launchd().Unload(plist))
			ref := string(jobRef(plist))
			if want := []string{"state " + ref, "state " + ref, "unload " + ref, "unload " + ref}; !slices.Equal(sched.all(), want) {
				t.Errorf("$HOME %q: scheduler calls %q, want %q", userHome, sched.all(), want)
			}
			if !slices.Equal(sched.sites, slices.Repeat(sched.sites[:1], len(sched.sites))) {
				t.Errorf("$HOME %q: %s is addressed at sites %q", userHome, plist, sched.sites)
			}
		}
	}
}

// siteRecorder is a fakeScheduler that also records the user home of each
// call's site, in order.
type siteRecorder struct {
	*fakeScheduler
	sites []string
}

func (s *siteRecorder) jobState(ctx context.Context, site schedulerSite, ref schedulerRef) string {
	s.sites = append(s.sites, site.userHome)
	return s.fakeScheduler.jobState(ctx, site, ref)
}

func (s *siteRecorder) load(ctx context.Context, site schedulerSite, ref schedulerRef) error {
	s.sites = append(s.sites, site.userHome)
	return s.fakeScheduler.load(ctx, site, ref)
}

func (s *siteRecorder) unload(ctx context.Context, site schedulerSite, ref schedulerRef) error {
	s.sites = append(s.sites, site.userHome)
	return s.fakeScheduler.unload(ctx, site, ref)
}

// launchdScheduler's load and unload run on a bounded context of their own
// that the caller's cancellation and deadline never reach: a setup that was
// interrupted (or whose own context ran out) still finishes the launchctl
// change it started, and the setup journal handles what comes after.
func TestLaunchdSchedulerChangesIgnoreTheCallersCancellation(t *testing.T) {
	site := schedulerSite{"/Users/me"}
	ref := schedulerRef("com.agent-archive.collector")
	plist := site.launchAgent(ref)
	type seen struct {
		err  error
		left time.Duration
	}
	calls := map[string]seen{}
	stubLaunchctlContext(t, func(ctx context.Context, args ...string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Errorf("launchctl %s ran with no deadline", args[0])
		}
		calls[args[0]] = seen{ctx.Err(), time.Until(deadline)}
		if args[0] == "print" {
			return []byte("path = " + plist + "\nstate = running\n"), nil
		}
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	cancel()

	must(t, launchdScheduler{}.load(ctx, site, ref))
	must(t, launchdScheduler{}.unload(ctx, site, ref))

	for _, tc := range []struct {
		verb string
		min  time.Duration
		max  time.Duration
	}{
		{"print", time.Second, 2 * time.Second},
		{"bootstrap", 25 * time.Second, launchctlChangeTimeout},
		{"bootout", 25 * time.Second, launchctlChangeTimeout},
	} {
		got, ok := calls[tc.verb]
		if !ok || got.err != nil || got.left < tc.min || got.left > tc.max {
			t.Errorf("launchctl %s: ran %v, context error %v, %v to its deadline; want it run, uncancelled, with between %v and %v", tc.verb, ok, got.err, got.left, tc.min, tc.max)
		}
	}

	// Asking changes nothing, so a question alone is the caller's to cancel.
	delete(calls, "print")
	launchdScheduler{}.jobState(ctx, site, ref)
	if got := calls["print"]; got.err == nil {
		t.Errorf("launchctl print for jobState ran uncancelled with %v to its deadline", got.left)
	}
}
