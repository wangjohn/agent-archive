package cli

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
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
	oldLabel := hooks.CollectorLabel(home+"-earlier", "")
	earlier := filepath.Join(userHome, "Library", "LaunchAgents", oldLabel+".plist")
	data, err := hooks.LaunchAgent("/opt/old/agent-archive", home, oldLabel, nil)
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

// setupjournal names jobs by plist path; envLaunchd reads each job's ref off
// the path and asks the scheduler about it at the user's home.
func TestEnvLaunchdNamesJobsByRefAtTheUsersHome(t *testing.T) {
	t.Parallel()
	sched := newFakeScheduler(t, "loaded")
	env, userHome := Env{Scheduler: sched}, filepath.Join("users", "me")
	launchd := env.launchd(userHome)
	plist := filepath.Join(userHome, "Library", "LaunchAgents", "com.agent-archive.collector.plist")
	if got := launchd.JobState(plist); got != "loaded" {
		t.Errorf("JobState = %q", got)
	}
	must(t, launchd.Unload(plist))
	if got := launchd.JobState(plist); got != "missing" {
		t.Errorf("JobState after Unload = %q", got)
	}
	must(t, launchd.Load(plist))
	want := []string{"state com.agent-archive.collector", "unload com.agent-archive.collector", "state com.agent-archive.collector", "load com.agent-archive.collector"}
	if got := sched.all(); !slices.Equal(got, want) {
		t.Errorf("scheduler calls %q, want %q", got, want)
	}
	if got := sched.state("com.agent-archive.collector"); got != "loaded" {
		t.Errorf("state after Load = %q", got)
	}
}
