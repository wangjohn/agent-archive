package systemd_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/testutil/schedulertest"
)

// realEnv names the environment variable that runs the tests of this file
// against the real systemd user manager of the user running them. They change
// that manager (units of the collector's own names, in the user's unit
// directory), so they run only where that is harmless: the CI job for it
// (.github/workflows/test.yml, real-systemd) on a disposable virtual machine,
// or a disposable container with systemd as PID 1 (dev/contributing/testing.md).
const realEnv = "AGENT_ARCHIVE_REAL_SYSTEMD"

// realHome is the home of the user running the tests: where their user
// manager looks for units.
func realHome(t *testing.T) string {
	t.Helper()
	if os.Getenv(realEnv) != "1" {
		t.Skip("set " + realEnv + "=1 to run against the real systemd user manager (a disposable machine only)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

// realManager is the real user manager behind the adapter under test, as the
// conformance suite's Manager: it puts a job in a state with systemctl itself
// (never through the adapter), reads the state back the same way, and counts
// what the adapter asked of it. The adapter's Runner is the real one, host.Exec.
type realManager struct {
	t     *testing.T
	home  string
	sched systemd.Scheduler

	mu    sync.Mutex
	calls []string
}

// unitDir is where the adapter writes units and the user manager looks for
// them; controlDir is where it looks first (what `systemctl --user edit --full`
// writes), so a unit there is one the manager runs in place of the adapter's.
func (m *realManager) unitDir() string { return filepath.Join(m.home, ".config", "systemd", "user") }

func (m *realManager) controlDir() string {
	return filepath.Join(m.home, ".config", "systemd", "user.control")
}

// run is the adapter's Runner: the real one, recorded.
func (m *realManager) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	m.mu.Unlock()
	return host.Exec(ctx, name, args...)
}

// sys runs systemctl --user args, for the manager's own setup, and returns
// what it printed and whether it failed.
func (m *realManager) sys(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := host.Exec(ctx, "systemctl", append([]string{"--user"}, args...)...)
	return string(out), err
}

func (m *realManager) mustSys(args ...string) {
	m.t.Helper()
	if out, err := m.sys(args...); err != nil {
		m.t.Fatalf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// foreignUnits are the units of the collector's names that exist now, in both
// unit directories, for a sweep to remove and the guard to refuse.
func (m *realManager) foreignUnits() []string {
	var found []string
	for _, dir := range []string{m.unitDir(), m.controlDir()} {
		matches, _ := filepath.Glob(filepath.Join(dir, "agent-archive-collector*"))
		found = append(found, matches...)
	}
	return found
}

// refuseForeignUnits is an error when the manager's unit directories hold units
// of the collector's names already (an installation of agent-archive), which a
// run would replace and its sweep remove.
func (m *realManager) refuseForeignUnits() error {
	if found := m.foreignUnits(); len(found) > 0 {
		return fmt.Errorf("%v: this machine has agent-archive units, and the real-manager run would remove them; run it on a disposable machine", found)
	}
	return nil
}

// sweep stops and removes every unit of the collector's names, and every link
// that enabled one, so a run leaves the manager as it found it.
func (m *realManager) sweep() {
	for _, path := range m.foreignUnits() {
		name := filepath.Base(path)
		if strings.HasSuffix(name, ".timer") || strings.HasSuffix(name, ".service") {
			_, _ = m.sys("stop", name)
		}
	}
	for _, path := range m.foreignUnits() {
		_ = os.RemoveAll(path)
	}
	for _, dir := range []string{m.unitDir(), m.controlDir()} {
		links, _ := filepath.Glob(filepath.Join(dir, "*.wants", "agent-archive-collector*"))
		for _, link := range links {
			_ = os.Remove(link)
		}
	}
	_, _ = m.sys("daemon-reload")
	_, _ = m.sys("reset-failed")
}

// reset returns the job ref to "nothing loaded and nothing overriding it",
// with the unit files the suite wrote left where they are.
func (m *realManager) reset(ref scheduler.Ref) {
	m.t.Helper()
	for _, unit := range []string{string(ref) + ".timer", string(ref) + ".service"} {
		_, _ = m.sys("stop", unit)
		_ = os.Remove(filepath.Join(m.controlDir(), unit))
		_ = os.Remove(filepath.Join(m.controlDir(), "timers.target.wants", unit))
		_ = os.Remove(filepath.Join(m.unitDir(), "timers.target.wants", unit))
	}
	m.mustSys("daemon-reload")
	_, _ = m.sys("reset-failed")
}

// program makes the executable the job's unit runs a script that does body, so
// the timer has something real to start: a collection that ends at once, or
// one that goes on.
func (m *realManager) program(site scheduler.Site, ref scheduler.Ref, body string) {
	m.t.Helper()
	def := m.sched.Definition(site, ref)
	if def.Program == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(def.Program), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(def.Program, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if def.DataHome != "" {
		if err := os.MkdirAll(def.DataHome, 0o700); err != nil {
			m.t.Fatal(err)
		}
	}
}

// loadState and activeState are the LoadState and ActiveState of a unit, as
// systemctl show prints them; the ones the state map tells apart.
type (
	loadState   string
	activeState string
)

const (
	loadLoaded loadState = "loaded"
	loadMasked loadState = "masked"

	stateActive       activeState = "active"
	stateActivating   activeState = "activating"
	stateDeactivating activeState = "deactivating"
)

// unitShow is what systemctl show says about one unit.
type unitShow struct {
	load     loadState
	active   activeState
	fragment string
}

// show reads the timer's and the service's properties from the manager.
func (m *realManager) show(ref scheduler.Ref) (timer, service unitShow) {
	m.t.Helper()
	out, err := m.sys("show", string(ref)+".timer", string(ref)+".service", "--property=Id,LoadState,ActiveState,FragmentPath")
	if err != nil {
		m.t.Fatalf("systemctl --user show: %v: %s", err, out)
	}
	units := map[string]unitShow{}
	var id string
	var current unitShow
	flush := func() {
		if id != "" {
			units[id] = current
		}
		id, current = "", unitShow{}
	}
	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			flush()
			continue
		}
		//lint:ignore LV1001 property names come from systemctl's output; any other property is skipped
		switch key {
		case "Id":
			id = value
		case "LoadState":
			current.load = loadState(value)
		case "ActiveState":
			current.active = activeState(value)
		case "FragmentPath":
			current.fragment = value
		}
	}
	flush()
	return units[string(ref)+".timer"], units[string(ref)+".service"]
}

// settle waits until the service of ref is not starting or stopping, so a
// state read right after it is the state of the timer alone.
func (m *realManager) settle(ref scheduler.Ref, want func(service unitShow) bool) {
	m.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, service := m.show(ref); want(service) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	timer, service := m.show(ref)
	m.t.Fatalf("the job %s did not settle: timer %+v, service %+v", ref, timer, service)
}

// Put makes the real manager hold the job ref at site as state.
func (m *realManager) Put(site scheduler.Site, ref scheduler.Ref, state scheduler.JobState) {
	m.t.Helper()
	m.reset(ref)
	timer, service := string(ref)+".timer", string(ref)+".service"
	switch state {
	case scheduler.Missing:
	case scheduler.Loaded:
		m.program(site, ref, "exit 0")
		m.mustSys("enable", "--now", timer)
		m.settle(ref, func(s unitShow) bool { return s.active != stateActivating && s.active != stateDeactivating })
	case scheduler.Running:
		m.program(site, ref, "exec sleep 600")
		m.mustSys("enable", "--now", timer)
		m.mustSys("start", "--no-block", service)
		m.settle(ref, func(s unitShow) bool { return s.active == stateActive || s.active == stateActivating })
	case scheduler.AnotherInstallation:
		// The same job, from files in the directory the manager reads first:
		// another installation's, as far as the manager can tell.
		m.program(site, ref, "exit 0")
		if err := os.MkdirAll(m.controlDir(), 0o755); err != nil {
			m.t.Fatal(err)
		}
		for _, unit := range []string{timer, service} {
			data, err := os.ReadFile(filepath.Join(m.unitDir(), unit))
			if err != nil {
				m.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(m.controlDir(), unit), data, 0o644); err != nil {
				m.t.Fatal(err)
			}
		}
		m.mustSys("daemon-reload")
		m.mustSys("enable", "--now", timer)
		m.settle(ref, func(s unitShow) bool { return s.active != stateActivating && s.active != stateDeactivating })
	case scheduler.Unknown:
		// A unit masked in the directory the manager reads first: it cannot
		// load the job, and says so.
		if err := os.MkdirAll(m.controlDir(), 0o755); err != nil {
			m.t.Fatal(err)
		}
		if err := os.Symlink("/dev/null", filepath.Join(m.controlDir(), timer)); err != nil {
			m.t.Fatal(err)
		}
		m.mustSys("daemon-reload")
	}
}

// Held is the state the real manager holds for the job, read from systemctl
// show by the state map written out again: a masked unit is unknown, a unit
// loaded from a file that is not the adapter's is another installation's, a
// service that is starting or running is running, an active timer is loaded, and
// anything else is missing.
func (m *realManager) Held(site scheduler.Site, ref scheduler.Ref) scheduler.JobState {
	m.t.Helper()
	timer, service := m.show(ref)
	ours := filepath.Join(m.unitDir(), string(ref)+".timer")
	switch {
	case timer.load == loadMasked || service.load == loadMasked:
		return scheduler.Unknown
	case timer.load == loadLoaded && !sameFile(timer.fragment, ours):
		return scheduler.AnotherInstallation
	case slices.Contains([]activeState{stateActive, stateActivating, stateDeactivating}, service.active):
		return scheduler.Running
	case timer.active == stateActive:
		return scheduler.Loaded
	}
	return scheduler.Missing
}

func sameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// Calls is what the adapter ran, in order.
func (m *realManager) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

// newRealManager is a real manager with nothing of the collector's in it, and
// a scheduler over the real Runner. It refuses to start on a machine that has
// units of the collector's names (an installation of agent-archive), which a
// run would replace and then remove; it sweeps its own before and after.
func newRealManager(t *testing.T, home string) *realManager {
	t.Helper()
	m := &realManager{t: t, home: home}
	m.sched = systemd.Scheduler{Run: m.run}
	if err := m.refuseForeignUnits(); err != nil {
		t.Fatal(err)
	}
	if out, err := m.sys("show-environment"); err != nil {
		t.Fatalf("there is no systemd user manager to talk to (systemctl --user show-environment: %v: %s). Run loginctl enable-linger, and set XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS", err, out)
	}
	t.Cleanup(m.sweep)
	return m
}

// The run over the real manager refuses a machine with units of the collector's
// names in either directory the manager reads, which its sweep would remove,
// and skips without its variable set to 1. Neither asks the manager anything.
func TestTheRealManagerRunIsGuarded(t *testing.T) {
	home := t.TempDir()
	m := &realManager{t: t, home: home}
	if err := m.refuseForeignUnits(); err != nil {
		t.Fatalf("a home with no units: %v", err)
	}
	for _, dir := range []string{m.unitDir(), m.controlDir()} {
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(os.MkdirAll(dir, 0o755))
		unit := filepath.Join(dir, "agent-archive-collector-0123456789ab.timer")
		must(os.WriteFile(unit, nil, 0o644))
		if err := m.refuseForeignUnits(); err == nil || !strings.Contains(err.Error(), unit) {
			t.Errorf("a home with %s: %v, want a refusal that names it", unit, err)
		}
		must(os.Remove(unit))
	}

	for _, value := range []string{"", "0", "true"} {
		t.Setenv(realEnv, value)
		var skipped bool
		t.Run("gate", func(t *testing.T) {
			defer func() { skipped = t.Skipped() }()
			realHome(t)
		})
		if !skipped {
			t.Errorf("%s=%q runs the tests over the real manager", realEnv, value)
		}
	}
}

// The conformance suite, over the real user manager: what the fake systemctl
// of the other conformance run is only a model of. The adapter's unit files
// are loaded, started, stopped and read back by the real thing, and the states
// the suite puts a job in (loaded, running, missing, another installation's,
// one the manager cannot describe) are real ones.
func TestRealUserManagerConformance(t *testing.T) {
	home := realHome(t)
	if out, err := host.Exec(context.Background(), "systemctl", "--version"); err == nil {
		t.Logf("%s", strings.SplitN(string(out), "\n", 2)[0])
	}
	schedulertest.RunConformance(t, schedulertest.Backend{
		New: func(t *testing.T) (scheduler.Scheduler, schedulertest.Manager) {
			t.Helper()
			m := newRealManager(t, home)
			return m.sched, m
		},
		Home: func(t *testing.T) string {
			t.Helper()
			return home
		},
		Golden: func(t *testing.T, name string, got []byte) {
			t.Helper()
			file := name + ".service"
			if timer, ok := strings.CutSuffix(name, ".1"); ok {
				file = timer + ".timer"
			}
			golden.Check(t, filepath.Join("testdata", "conformance", file), got)
		},
		Skip: map[string]string{
			// A real manager cannot hold a loaded job whose definition was
			// damaged after it loaded: the suite damages the files, and then
			// asks the manager to hold the job as loaded.
			"UnreadableAndAbsentDefinitions": "a real manager cannot be put in a loaded state over damaged unit files",
		},
	})
}

// The adapter's Runner (host.Exec) gives systemctl the environment it needs:
// with the colors the user's environment forces (SYSTEMD_COLORS=true colors
// even a pipe's output, and the version line starts with an escape sequence, so
// the adapter would not read it), `systemctl --version` still starts "systemd ",
// and the adapter asks about the job as usual.
func TestRealSystemctlVersionParsesWhateverTheTerminalSays(t *testing.T) {
	home := realHome(t)
	t.Setenv("SYSTEMD_COLORS", "true")
	out, err := host.Exec(context.Background(), "systemctl", "--version")
	if err != nil || !strings.HasPrefix(string(out), "systemd ") {
		t.Fatalf("systemctl --version through the real Runner: %v: %q, want a line that starts \"systemd \"", err, out)
	}
	m := newRealManager(t, home)
	site := scheduler.Site{UserHome: home}
	status := m.sched.Inspect(context.Background(), site, "agent-archive-collector")
	if status.State != scheduler.Missing || status.Problem != nil {
		t.Fatalf("Inspect of a job that was never defined: %q, %+v, want missing", status.State, status.Problem)
	}
	if calls := m.Calls(); len(calls) < 2 || calls[0] != "systemctl --version" {
		t.Errorf("the adapter asked %q", calls)
	}
}
