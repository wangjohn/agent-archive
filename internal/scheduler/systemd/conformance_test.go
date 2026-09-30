package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
	"github.com/wangjohn/agent-archive/internal/testutil/schedulertest"
)

// fixtures are `systemctl show` and `systemctl --version` outputs captured
// from real user managers of systemd 239 (Rocky 8), 245 (Ubuntu 20.04), 252
// (Debian 12) and 255 (Ubuntu 24.04), in testdata/systemctl (see the README
// there for how). @REF@, @TIMER@, @SERVICE@ and @OTHER@ stand for the job,
// its two unit files and another user's unit directory.
const fixtures = "testdata/systemctl"

// fakeSystemctl is the systemctl (and loginctl) the conformance suite drives
// the adapter through: a Runner that answers `show` with the fixture of the
// state the suite put the job in, and enables and disables a timer as
// systemd does. Like the real one it fails when its context is done (as
// exec.CommandContext does); disable and stop stop whatever job has the name,
// whichever unit file it was loaded from (systemd does not check, the adapter
// must); and it finds a unit by name in the site's unit directory, as systemd
// does, so it is told the site (UseSite). It is the schedulertest.Manager of
// the adapter.
type fakeSystemctl struct {
	t  *testing.T
	mu sync.Mutex
	// version is the fixture directory `--version` is answered from, and
	// shown the one `show` is (a test may set another); versionText, when
	// set, is the answer to `--version` instead.
	version     string
	shown       string
	versionText string
	// override answers every show with this fixture, whatever was Put.
	override string
	// linger is what loginctl says about lingering.
	linger string
	site   scheduler.Site
	state  map[string]scheduler.JobState
	calls  []string
}

func newFakeSystemctl(t *testing.T, version string) *fakeSystemctl {
	t.Helper()
	return &fakeSystemctl{t: t, version: version, shown: version, linger: "yes", state: map[string]scheduler.JobState{}}
}

func (f *fakeSystemctl) fixture(name string) string {
	f.t.Helper()
	dir := f.shown
	if name == "version.txt" {
		dir = f.version
	}
	data, err := os.ReadFile(filepath.Join(fixtures, dir, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *fakeSystemctl) show(ref string) ([]byte, error) {
	name := map[scheduler.JobState]string{scheduler.Loaded: "loaded", scheduler.Running: "running", scheduler.AnotherInstallation: "another", scheduler.Missing: "missing"}[f.held(ref)]
	if f.override != "" {
		name = f.override
	}
	if name == "" { // the manager cannot be reached
		return []byte("Failed to connect to bus: No medium found\n"), errors.New("exit status 1")
	}
	site := Scheduler{}
	return []byte(strings.NewReplacer("@REF@", ref, "@TIMER@", site.timerPath(f.site, scheduler.Ref(ref)), "@SERVICE@", site.servicePath(f.site, scheduler.Ref(ref)), "@OTHER@", "/home/someone/.config/systemd/user").Replace(f.fixture("show-" + name + ".txt"))), nil
}

func (f *fakeSystemctl) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case name == "loginctl" && len(args) == 3 && args[0] == "show-user":
		return []byte("Linger=" + f.linger + "\n"), nil
	case name != "systemctl":
	case len(args) == 1 && args[0] == "--version" && f.versionText != "":
		return []byte(f.versionText), nil
	case len(args) == 1 && args[0] == "--version":
		return []byte(f.fixture("version.txt")), nil
	case len(args) == 5 && args[0] == "--user" && args[1] == "show":
		return f.show(strings.TrimSuffix(args[2], ".timer"))
	case len(args) == 2 && args[0] == "--user" && args[1] == "daemon-reload":
		return nil, nil
	case len(args) == 4 && args[0] == "--user" && args[1] == "enable" && args[2] == "--now":
		ref := strings.TrimSuffix(args[3], ".timer")
		for _, path := range []string{Scheduler{}.timerPath(f.site, scheduler.Ref(ref)), Scheduler{}.servicePath(f.site, scheduler.Ref(ref))} {
			if _, err := os.Stat(path); err != nil {
				return []byte("Failed to enable unit: Unit file " + args[3] + " does not exist."), errors.New("exit status 1")
			}
		}
		f.state[ref] = scheduler.Loaded
		return nil, nil
	case len(args) == 4 && args[0] == "--user" && args[1] == "disable" && args[2] == "--now":
		delete(f.state, strings.TrimSuffix(args[3], ".timer"))
		return nil, nil
	case len(args) == 3 && args[0] == "--user" && (args[1] == "stop" || args[1] == "reset-failed"):
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected %s %q", name, args)
}

func (f *fakeSystemctl) held(ref string) scheduler.JobState {
	if state, ok := f.state[ref]; ok {
		return state
	}
	return scheduler.Missing
}

func (f *fakeSystemctl) UseSite(site scheduler.Site) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.site = site
}

func (f *fakeSystemctl) Put(site scheduler.Site, ref scheduler.Ref, state scheduler.JobState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.site, f.state[string(ref)] = site, state
}

func (f *fakeSystemctl) Held(_ scheduler.Site, ref scheduler.Ref) scheduler.JobState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held(string(ref))
}

func (f *fakeSystemctl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// systemd passes the conformance suite over a systemctl that shows what the
// fixtures say, for each captured systemd (239 is RHEL 8's, which has the
// StandardOutput=append: the unit needs; an older 239 is refused, see
// TestOldSystemdIsRefused), and its unit files are the
// golden files in testdata/conformance: <name>.service and <name>.timer. It
// is not parallel: one of the suite's checks sets the process's environment.
func TestConformance(t *testing.T) {
	for _, version := range []string{"239", "245", "252", "255"} {
		t.Run("systemd"+version, func(t *testing.T) {
			schedulertest.RunConformance(t, schedulertest.Backend{
				New: func(t *testing.T) (scheduler.Scheduler, schedulertest.Manager) {
					t.Helper()
					f := newFakeSystemctl(t, version)
					return Scheduler{Run: f.run}, f
				},
				Golden: func(t *testing.T, name string, got []byte) {
					t.Helper()
					file := name + ".service"
					if timer, ok := strings.CutSuffix(name, ".1"); ok {
						file = timer + ".timer"
					}
					golden.Check(t, filepath.Join("testdata", "conformance", file), got)
				},
			})
		})
	}
}
