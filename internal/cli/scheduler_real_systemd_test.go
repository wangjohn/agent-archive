package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
)

// realSystemdEnv runs the tests below against the real systemd user manager of
// the user running them, which they change (a unit of the collector's names in
// the user's unit directory): only on a disposable machine. The CI job for them
// is real-systemd in .github/workflows/test.yml; internal/scheduler/systemd has
// the conformance run over the same manager.
const realSystemdEnv = "AGENT_ARCHIVE_REAL_SYSTEMD"

// requireRealSystemd skips t unless realSystemdEnv is 1.
func requireRealSystemd(t *testing.T) {
	t.Helper()
	if os.Getenv(realSystemdEnv) != "1" {
		t.Skip("set " + realSystemdEnv + "=1 to run against the real systemd user manager (a disposable machine only)")
	}
}

// Without realSystemdEnv set to 1 the tests over the real manager skip, so no
// ordinary go test (a developer's Linux login, the Ubuntu test job) runs setup
// and uninstall against the running user's manager and home.
func TestTheRealManagerTestsSkipWithoutTheirVariable(t *testing.T) {
	for _, value := range []string{"", "0", "true", "yes"} {
		t.Setenv(realSystemdEnv, value)
		var skipped bool
		t.Run("gate", func(t *testing.T) {
			defer func() { skipped = t.Skipped() }()
			requireRealSystemd(t)
		})
		if !skipped {
			t.Errorf("%s=%q runs the tests over the real manager", realSystemdEnv, value)
		}
	}
}

// realSystemdInstall is an installation set up by the real setup, with the
// real adapter, under the real user manager.
type realSystemdInstall struct {
	env      Env
	home     string
	realHome string
	unitDir  string
	ref      string
	// fired is the file the job's program writes a line to each time the
	// timer starts it.
	fired string
	// unreachable makes every systemctl and loginctl the adapter runs fail
	// as it does in a session with no user bus.
	unreachable atomic.Bool
}

// newRealSystemdInstall runs the real setup for a data directory of its own,
// over the real user manager, and has the job's program be a stand-in that
// writes down each time it is asked to collect. It skips unless realSystemdEnv
// is 1, and refuses a machine that has units of the collector's names.
func newRealSystemdInstall(t *testing.T) *realSystemdInstall {
	t.Helper()
	requireRealSystemd(t)
	account, err := user.Current()
	must(t, err)
	r := &realSystemdInstall{realHome: account.HomeDir, home: t.TempDir()}
	r.unitDir = filepath.Join(r.realHome, ".config", "systemd", "user")
	if existing, _ := filepath.Glob(filepath.Join(r.unitDir, "agent-archive-collector*")); len(existing) > 0 {
		t.Fatalf("%v: this machine has agent-archive units, which the smoke test would remove; run it on a disposable machine", existing)
	}

	r.env = setupTestEnv(t, r.home, r.realHome, newFakeKeychain(), time.Now())
	r.env.OS = platform.Linux
	r.env.Scheduler = host.New(platform.Linux, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if r.unreachable.Load() {
			return []byte("Failed to connect to user scope bus via local transport: No such file or directory\n"), errors.New("exit status 1")
		}
		return host.Exec(ctx, name, args...)
	})
	// Not the account's default installation, so its job has a name of its own.
	r.env.AccountHome = func() (string, error) { return t.TempDir(), nil }
	r.ref = string(r.env.installation(r.home, r.realHome).ref())
	t.Cleanup(func() { removeRealUnits(r.ref) })

	// The program the timer starts: it says each time it is asked to collect.
	r.fired = filepath.Join(r.home, "fired")
	program := filepath.Join(r.realHome, "bin", "agent-archive-smoke-"+r.ref)
	must(t, os.MkdirAll(filepath.Dir(program), 0o755))
	must(t, os.WriteFile(program, []byte("#!/bin/sh\n[ \"$1\" = _collect ] && echo \"$1 $AGENT_ARCHIVE_HOME\" >> '"+r.fired+"'\nexit 0\n"), 0o755))
	t.Cleanup(func() { _ = os.Remove(program) })
	r.env.Executable = func() (string, error) { return program, nil }

	setupRun(t, r.env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()), 0)
	if cfg := mustLoadConfig(t, r.home); cfg.BackgroundBackend != "systemd" {
		t.Errorf("setup recorded the backend %q, want systemd", cfg.BackgroundBackend)
	}
	for _, unit := range []string{r.ref + ".service", r.ref + ".timer"} {
		if info, err := os.Stat(filepath.Join(r.unitDir, unit)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want a private unit file", unit, info, err)
		}
	}
	if got := scheduler.JobState(backgroundState(t, r.env)); !got.Active() {
		t.Fatalf("status says the background job is %q after setup, want it loaded", got)
	}
	return r
}

// uninstall runs uninstall --yes with args, and returns its exit code and
// what it printed to both streams.
func (r *realSystemdInstall) uninstall(args ...string) (int, string) {
	var out, errOut bytes.Buffer
	code := Run(append([]string{"uninstall", "--yes"}, args...), strings.NewReader(""), &out, &errOut, r.env)
	return code, out.String() + errOut.String()
}

// unitsGone says what uninstall left of the job's unit files.
func (r *realSystemdInstall) unitsGone(t *testing.T) {
	t.Helper()
	for _, unit := range []string{r.ref + ".service", r.ref + ".timer"} {
		if _, err := os.Stat(filepath.Join(r.unitDir, unit)); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s (%v)", unit, err)
		}
	}
}

// staysStill fails when the job's program runs within a few seconds.
func (r *realSystemdInstall) staysStill(t *testing.T) {
	t.Helper()
	before, _ := os.ReadFile(r.fired)
	time.Sleep(3 * time.Second)
	if after, _ := os.ReadFile(r.fired); !bytes.Equal(before, after) {
		t.Errorf("the job ran after uninstall:\n%s", after)
	}
}

// A setup-level smoke over the real user manager: the real commands, with the
// real adapter and the real Runner, install the collector for a data directory
// of its own, the manager's timer starts the program `_collect` (a stand-in
// script that writes down each time it runs) on its own, and uninstall leaves
// the manager with nothing. Storage, the keychain and the apps are the usual
// stand-ins; the scheduler is what is real.
func TestRealSystemdSetupRunsTheTimerAndUninstallStopsIt(t *testing.T) {
	// Not parallel: each installs the collector's units in the one real user
	// manager, and newRealSystemdInstall refuses a manager that already has some.
	r := newRealSystemdInstall(t)

	// The manager starts the timer's first run by itself: a minute after the
	// user manager started, so wait for it.
	deadline := time.Now().Add(150 * time.Second)
	for {
		if data, _ := os.ReadFile(r.fired); strings.Contains(string(data), "_collect "+r.home) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the timer did not start its program in 150 seconds; the service says:\n%s", realSystemctl(t, "status", "--no-pager", r.ref+".service", r.ref+".timer"))
		}
		time.Sleep(2 * time.Second)
	}

	if code, output := r.uninstall(); code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, output)
	}
	r.unitsGone(t)
	if got := backgroundState(t, r.env); got != "missing" {
		t.Errorf("status says the background job is %q after uninstall, want missing", got)
	}
	if listed := realSystemctl(t, "list-units", "--all", "--no-legend", r.ref+".timer", r.ref+".service"); strings.TrimSpace(listed) != "" {
		t.Errorf("the manager still lists the job after uninstall:\n%s", listed)
	}
	// Nothing runs it any more.
	r.staysStill(t)
}

// uninstall --skip-scheduler from a session that cannot reach the manager
// deletes the unit files under a timer the manager still runs, and says so; the
// command it prints for stopping the job by hand, run from a session that can
// reach the manager, stops it with the files already gone.
func TestRealSystemdUninstallSkippingTheSchedulerPrintsACommandThatStopsTheJob(t *testing.T) {
	// Not parallel: each installs the collector's units in the one real user
	// manager, and newRealSystemdInstall refuses a manager that already has some.
	r := newRealSystemdInstall(t)
	timer := r.ref + ".timer"

	r.unreachable.Store(true)
	code, output := r.uninstall("--skip-scheduler")
	r.unreachable.Store(false)
	if code != 0 || !strings.Contains(output, "Not verified stopped: systemd's "+r.ref+" job may still be running") {
		t.Fatalf("uninstall --skip-scheduler: exit %d\n%s", code, output)
	}
	r.unitsGone(t)
	// What the summary warns of: the manager still runs the timer.
	if active := strings.TrimSpace(realSystemctl(t, "is-active", timer)); active != "active" {
		t.Errorf("the timer is %q after uninstall --skip-scheduler deleted its files; want it still active, as the summary says it may be", active)
	}

	const prefix = "To stop it, run this from a session that can reach systemd: "
	var manual string
	for line := range strings.SplitSeq(output, "\n") {
		if command, ok := strings.CutPrefix(line, prefix); ok {
			manual = command
		}
	}
	if manual == "" {
		t.Fatalf("uninstall --skip-scheduler printed no command to stop the job:\n%s", output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// As the person would: in a shell of theirs.
	if out, err := exec.CommandContext(ctx, "sh", "-c", manual).CombinedOutput(); err != nil {
		t.Fatalf("the printed command %q failed: %v\n%s", manual, err, out)
	}
	// is-active exits 0 when either unit is active; the service's runs, which
	// a oneshot spends activating, are what staysStill watches for.
	if _, err := host.Exec(ctx, "systemctl", "--user", "is-active", "--quiet", timer, r.ref+".service"); err == nil {
		t.Errorf("the job is still active after the printed command %q:\n%s", manual, realSystemctl(t, "status", "--no-pager", timer, r.ref+".service"))
	}
	r.staysStill(t)
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
// service stopped and disabled, their files and the timer's enabling link gone,
// the manager reloaded. The stop comes first and by name, since systemctl
// refuses to disable a unit whose file is gone.
func removeRealUnits(ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(args ...string) { _, _ = host.Exec(ctx, "systemctl", append([]string{"--user"}, args...)...) }
	run("stop", ref+".timer", ref+".service")
	run("disable", ref+".timer")
	if account, err := user.Current(); err == nil {
		unitDir := filepath.Join(account.HomeDir, ".config", "systemd", "user")
		for _, file := range []string{ref + ".service", ref + ".timer", filepath.Join("timers.target.wants", ref+".timer")} {
			_ = os.Remove(filepath.Join(unitDir, file))
		}
	}
	run("daemon-reload")
	run("reset-failed")
}
