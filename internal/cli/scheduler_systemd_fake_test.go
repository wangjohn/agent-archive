package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/systemd"
)

// fakeUserManager is a user's systemd manager, as far as the commands ask it
// (systemctl --user and loginctl through the systemd adapter's Runner): it
// keeps the state of each job by name, answers `show` the way a real manager
// does (fixtures of real ones are in internal/scheduler/systemd/testdata),
// enables a timer only when both unit files are where the user manager
// searches, and records every call, in order, as the command line it was
// given.
type fakeUserManager struct {
	t *testing.T
	// userHome is the home whose .config/systemd/user holds the unit files.
	userHome string

	mu    sync.Mutex
	calls []string
	state map[string]scheduler.JobState
	// version is what `systemctl --version` prints; linger is what loginctl
	// says about lingering ("yes" or "no").
	version string
	linger  string
	// noBus makes every systemctl fail as it does with no user manager to
	// talk to (an ssh session without pam_systemd, a container).
	noBus bool
	// failLoad, when set, is the output of an `enable --now` that fails.
	failLoad string
	// searchHome, when set, is the home the manager searches for unit files
	// in place of userHome: a manager whose own environment sets
	// XDG_CONFIG_HOME does not look where the adapter wrote them.
	searchHome string
	// dropIn makes the timer report a drop-in that overrides it.
	dropIn bool
	// masked makes both units of every job masked.
	masked bool
}

const fakeSystemdVersion = "systemd 255 (255.4-1ubuntu8.17)\n+PAM +AUDIT +SELINUX default-hierarchy=unified\n"

func newFakeUserManager(t *testing.T, userHome string) *fakeUserManager {
	t.Helper()
	return &fakeUserManager{t: t, userHome: userHome, version: fakeSystemdVersion, linger: "yes", state: map[string]scheduler.JobState{}}
}

// scheduler is the systemd adapter over the manager.
func (m *fakeUserManager) scheduler() systemd.Scheduler { return systemd.Scheduler{Run: m.run} }

// units are the two unit files of job ref, where the user manager searches.
func (m *fakeUserManager) units(ref string) (timer, service string) {
	home := m.userHome
	if m.searchHome != "" {
		home = m.searchHome
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	return filepath.Join(dir, ref+".timer"), filepath.Join(dir, ref+".service")
}

// put sets the state of the job called ref.
func (m *fakeUserManager) put(ref string, state scheduler.JobState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[ref] = state
}

func (m *fakeUserManager) held(ref string) scheduler.JobState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.state[ref]; ok {
		return state
	}
	return scheduler.Missing
}

// all is every systemctl and loginctl call so far, in order.
func (m *fakeUserManager) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

// changing is the calls that change something: not the questions.
func (m *fakeUserManager) changing() []string {
	var out []string
	for _, call := range m.all() {
		if !strings.HasPrefix(call, "systemctl --version") && !strings.Contains(call, " show ") && !strings.HasPrefix(call, "loginctl ") {
			out = append(out, call)
		}
	}
	return out
}

func (m *fakeUserManager) show(ref string) string {
	timer, service := m.units(ref)
	dropIns := ""
	if m.dropIn {
		dropIns = timer + ".d/override.conf"
	}
	block := func(id, path, load, active, sub string) string {
		in := ""
		if strings.HasSuffix(id, ".timer") {
			in = dropIns
		}
		return fmt.Sprintf("Id=%s\nLoadState=%s\nActiveState=%s\nSubState=%s\nFragmentPath=%s\nDropInPaths=%s\n", id, load, active, sub, path, in)
	}
	if m.masked {
		return block(ref+".timer", "", "masked", "inactive", "dead") + "\n" + block(ref+".service", "", "masked", "inactive", "dead")
	}
	switch m.held(ref) {
	case scheduler.Loaded:
		return block(ref+".timer", timer, "loaded", "active", "waiting") + "\n" + block(ref+".service", service, "loaded", "inactive", "dead")
	case scheduler.Running:
		return block(ref+".timer", timer, "loaded", "active", "waiting") + "\n" + block(ref+".service", service, "loaded", "activating", "start")
	case scheduler.AnotherInstallation:
		other := "/home/someone/.config/systemd/user/" + ref
		return block(ref+".timer", other+".timer", "loaded", "active", "waiting") + "\n" + block(ref+".service", other+".service", "loaded", "inactive", "dead")
	case scheduler.Missing, scheduler.Unknown:
	}
	return block(ref+".timer", "", "not-found", "inactive", "dead") + "\n" + block(ref+".service", "", "not-found", "inactive", "dead")
}

func (m *fakeUserManager) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, name+" "+strings.Join(args, " "))
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bus := []byte("Failed to connect to bus: No medium found\n")
	switch {
	case name == "loginctl" && len(args) == 3 && args[0] == "show-user":
		return []byte("Linger=" + m.linger + "\n"), nil
	case name != "systemctl":
	case len(args) == 1 && args[0] == "--version":
		return []byte(m.version), nil
	case m.noBus:
		return bus, errors.New("exit status 1")
	case len(args) == 5 && args[0] == "--user" && args[1] == "show":
		return []byte(m.show(strings.TrimSuffix(args[2], ".timer"))), nil
	case len(args) == 2 && args[0] == "--user" && args[1] == "daemon-reload":
		return nil, nil
	case len(args) == 4 && args[0] == "--user" && args[1] == "enable" && args[2] == "--now":
		ref := strings.TrimSuffix(args[3], ".timer")
		timer, service := m.units(ref)
		for _, path := range []string{timer, service} {
			if _, err := os.Stat(path); err != nil {
				return []byte("Failed to enable unit: Unit file " + args[3] + " does not exist.\n"), errors.New("exit status 1")
			}
		}
		if m.failLoad != "" {
			return []byte(m.failLoad), errors.New("exit status 1")
		}
		m.put(ref, scheduler.Loaded)
		return nil, nil
	case len(args) == 4 && args[0] == "--user" && args[1] == "disable" && args[2] == "--now":
		m.mu.Lock()
		delete(m.state, strings.TrimSuffix(args[3], ".timer"))
		m.mu.Unlock()
		return nil, nil
	case len(args) == 3 && args[0] == "--user" && (args[1] == "stop" || args[1] == "reset-failed"):
		return nil, nil
	}
	return nil, fmt.Errorf("the fake user manager was asked %s %q", name, args)
}

// linuxEnv is env as it is on Linux with a user manager: its systemd adapter
// over m, and the operating system Linux.
func (m *fakeUserManager) linuxEnv(env Env) Env {
	env.Scheduler = m.scheduler()
	env.OS = platform.Linux
	return env
}
