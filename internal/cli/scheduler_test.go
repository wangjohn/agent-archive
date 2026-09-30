package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// jobRef is the job the plist at path defines, as launchd names it.
func jobRef(plist string) scheduler.Ref { return scheduler.Ref(launchd.Label(plist)) }

// launchd finds a job's definition from its ref and the user home
// alone, so every plist a command names (this installation's own, an earlier
// release's under another label, the prototype's upload job) must be the one
// its ref and site give back.
func TestLaunchdFindsEveryPlistFromRefAndSite(t *testing.T) {
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
	site := scheduler.Site{UserHome: userHome}
	prototype := filepath.Join(userHome, "Library", "LaunchAgents", launchd.LegacyLaunchLabel+".plist")
	for _, plist := range []string{in.collectorPlist(), earlier, prototype} {
		if got := launchd.PlistPath(site, jobRef(plist)); got != plist {
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

	target := launchd.ServiceTarget(plist)
	want := []string{"print " + target, "print " + target, "bootout " + target, "bootstrap " + strings.TrimSuffix(target, "/"+label) + " " + plist}
	if !slices.Equal(argv, want) {
		t.Errorf("launchctl calls\n%q\nwant\n%q", argv, want)
	}
	if got := readText(t, plist); got != "old" {
		t.Errorf("the journal's plist reads %q after recovery", got)
	}
}

// setupjournal drives the scheduler by name: the one this system has, and no
// other. A journal that names another backend is refused, so its jobs are
// never stopped or started through the wrong one.
func TestBackendsResolveTheSystemsOwnScheduler(t *testing.T) {
	t.Parallel()
	sched := newFakeScheduler(t, "loaded")
	backends := Env{Scheduler: sched}.backends()
	got, err := backends(setupjournal.DefaultBackend)
	if err != nil || got != scheduler.Scheduler(sched) {
		t.Errorf("backends(%q) = %v, %v; want the Env's scheduler", setupjournal.DefaultBackend, got, err)
	}
	if got, err = backends("systemd"); err == nil || got != nil || !strings.Contains(err.Error(), "systemd") || !strings.Contains(err.Error(), sched.Name()) {
		t.Errorf("backends(systemd) = %v, %v; want a refusal naming both", got, err)
	}
	if len(sched.all()) != 0 {
		t.Errorf("resolving a backend asked the scheduler %q", sched.all())
	}
}

// A setup plans with the user home (Env.jobStatus) and commits and recovers
// with the definition its journal records (Locate), so both must name every
// plist a release journals as the same job at the same site, however $HOME is
// spelled.
func TestEveryJournaledPlistIsOneJobAtOneSite(t *testing.T) {
	t.Parallel()
	for _, userHome := range []string{"/Users/me", "/Users/me/", "/Users/me//", "/Users/./me/../me", "/", "me", "./me/", "."} {
		env := Env{Scheduler: newFakeScheduler(t, "loaded"), AccountHome: func() (string, error) { return "/Users/account", nil }}
		in := env.installation("/Users/me/archive", userHome)
		earlier := filepath.Join(userHome, "Library", "LaunchAgents", launchd.CollectorLabel("/Users/me/Archive", "")+".plist")
		prototype := filepath.Join(userHome, "Library", "LaunchAgents", launchd.LegacyLaunchLabel+".plist")
		for _, plist := range []string{in.collectorPlist(), earlier, prototype} {
			site, ref, err := env.scheduler().Locate(plist)
			if err != nil || site != userSite(userHome) || ref != jobRef(plist) {
				t.Errorf("$HOME %q: %s is the job %s at %+v (%v); a setup plans it at %+v", userHome, plist, ref, site, err, userSite(userHome))
			}
		}
	}
}
