package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
)

// realSystemdEnv runs the test below against the real systemd user manager of
// the user running it, which it changes (a unit of the collector's names in the
// user's unit directory): only on a disposable machine. The CI job for it is
// real-systemd in .github/workflows/test.yml; internal/scheduler/systemd has the
// conformance run over the same manager.
const realSystemdEnv = "AGENT_ARCHIVE_REAL_SYSTEMD"

// A setup-level smoke over the real user manager: the real commands, with the
// real adapter and the real Runner, install the collector for a data directory
// of its own, the manager's timer starts the program `_collect` (a stand-in
// script that writes down each time it runs) on its own, and uninstall leaves
// the manager with nothing. Storage, the keychain and the apps are the usual
// stand-ins; the scheduler is what is real.
func TestRealSystemdSetupRunsTheTimerAndUninstallStopsIt(t *testing.T) {
	if os.Getenv(realSystemdEnv) != "1" {
		t.Skip("set " + realSystemdEnv + "=1 to run against the real systemd user manager (a disposable machine only)")
	}
	account, err := user.Current()
	must(t, err)
	realHome := account.HomeDir
	unitDir := filepath.Join(realHome, ".config", "systemd", "user")
	if existing, _ := filepath.Glob(filepath.Join(unitDir, "agent-archive-collector*")); len(existing) > 0 {
		t.Fatalf("%v: this machine has agent-archive units, which the smoke test would remove; run it on a disposable machine", existing)
	}

	home := t.TempDir()
	env := setupTestEnv(t, home, realHome, newFakeKeychain(), time.Now())
	env.OS = platform.Linux
	env.Scheduler = host.New(platform.Linux, host.Exec)
	// Not the account's default installation, so its job has a name of its own.
	env.AccountHome = func() (string, error) { return t.TempDir(), nil }
	ref := string(env.installation(home, realHome).ref())
	t.Cleanup(func() { removeRealUnits(ref) })

	// The program the timer starts: it says each time it is asked to collect.
	fired := filepath.Join(home, "fired")
	program := filepath.Join(realHome, "bin", "agent-archive-smoke-"+ref)
	must(t, os.MkdirAll(filepath.Dir(program), 0o755))
	must(t, os.WriteFile(program, []byte("#!/bin/sh\n[ \"$1\" = _collect ] && echo \"$1 $AGENT_ARCHIVE_HOME\" >> '"+fired+"'\nexit 0\n"), 0o755))
	t.Cleanup(func() { _ = os.Remove(program) })
	env.Executable = func() (string, error) { return program, nil }

	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	if cfg := mustLoadConfig(t, home); cfg.BackgroundBackend != "systemd" {
		t.Errorf("setup recorded the backend %q, want systemd", cfg.BackgroundBackend)
	}
	for _, unit := range []string{ref + ".service", ref + ".timer"} {
		if info, err := os.Stat(filepath.Join(unitDir, unit)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want a private unit file", unit, info, err)
		}
	}
	if got := scheduler.JobState(backgroundState(t, env)); !got.Active() {
		t.Fatalf("status says the background job is %q after setup, want it loaded", got)
	}

	// The manager starts the timer's first run by itself: a minute after the
	// user manager started, so wait for it.
	deadline := time.Now().Add(150 * time.Second)
	for {
		if data, _ := os.ReadFile(fired); strings.Contains(string(data), "_collect "+home) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the timer did not start %s in 150 seconds; the service says:\n%s", program, realSystemctl(t, "status", "--no-pager", ref+".service", ref+".timer"))
		}
		time.Sleep(2 * time.Second)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall: exit %d\n%s%s", code, &out, &errOut)
	}
	for _, unit := range []string{ref + ".service", ref + ".timer"} {
		if _, err := os.Stat(filepath.Join(unitDir, unit)); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s (%v)", unit, err)
		}
	}
	if got := backgroundState(t, env); got != "missing" {
		t.Errorf("status says the background job is %q after uninstall, want missing", got)
	}
	if listed := realSystemctl(t, "list-units", "--all", "--no-legend", ref+".timer", ref+".service"); strings.TrimSpace(listed) != "" {
		t.Errorf("the manager still lists the job after uninstall:\n%s", listed)
	}
	// Nothing runs it any more.
	before, _ := os.ReadFile(fired)
	time.Sleep(3 * time.Second)
	if after, _ := os.ReadFile(fired); !bytes.Equal(before, after) {
		t.Errorf("the job ran after uninstall:\n%s", after)
	}
}

// backgroundState is what `status --json` says the background job is.
func backgroundState(t *testing.T, env Env) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("status --json: exit %d\n%s%s", code, &out, &errOut)
	}
	var status struct {
		Background string `json:"background"`
	}
	must(t, json.Unmarshal(out.Bytes(), &status))
	return status.Background
}

// realSystemctl runs systemctl --user with args against the real manager and
// returns what it printed, whatever its exit status.
func realSystemctl(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := host.Exec(ctx, "systemctl", append([]string{"--user"}, args...)...)
	return string(out)
}

// removeRealUnits leaves no trace of a job of the smoke test: its timer and
// service stopped and disabled, their files gone, the manager reloaded.
func removeRealUnits(ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(args ...string) { _, _ = host.Exec(ctx, "systemctl", append([]string{"--user"}, args...)...) }
	run("disable", "--now", ref+".timer")
	run("stop", ref+".service")
	if account, err := user.Current(); err == nil {
		for _, unit := range []string{ref + ".service", ref + ".timer"} {
			_ = os.Remove(filepath.Join(account.HomeDir, ".config", "systemd", "user", unit))
		}
	}
	run("daemon-reload")
	run("reset-failed")
}
