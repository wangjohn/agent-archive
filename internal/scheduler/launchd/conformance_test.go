package launchd

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

// recordings are the launchctl print outputs the characterization of the
// macOS scheduler recorded (see the README there); @PLIST@ stands for the
// plist launchd loaded the job from.
const recordings = "../../cli/testdata/scheduler/launchctl-print"

// fakeLaunchctl is the launchctl the conformance suite drives the adapter
// through: a Runner that answers print with the recorded output of the state
// the suite put the job in, boots a job out and bootstraps one as launchd
// does. Like the real one, it fails when its context is done (as
// exec.CommandContext does), and bootout stops whatever job has the label,
// whichever plist it was loaded from: launchd does not check, the adapter
// must. It is the schedulertest.Manager of the adapter.
type fakeLaunchctl struct {
	t     *testing.T
	mu    sync.Mutex
	state map[string]scheduler.JobState
	// plists are the plists the labels were put or bootstrapped from.
	plists map[string]string
	calls  []string
}

func newFakeLaunchctl(t *testing.T) *fakeLaunchctl {
	t.Helper()
	return &fakeLaunchctl{t: t, state: map[string]scheduler.JobState{}, plists: map[string]string{}}
}

func (f *fakeLaunchctl) recording(name, plist string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(recordings, name))
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "@PLIST@", plist)
}

func (f *fakeLaunchctl) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "launchctl" {
		return nil, fmt.Errorf("ran %q, not launchctl", name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(args) == 2 && args[0] == "print":
		label := args[1][strings.LastIndex(args[1], "/")+1:]
		plist := f.plists[label]
		switch f.held(label) {
		case scheduler.Running:
			return []byte(f.recording("running.txt", plist)), nil
		case scheduler.Loaded:
			return []byte(f.recording("loaded-not-running.txt", plist)), nil
		case scheduler.AnotherInstallation:
			return []byte(f.recording("another-installation.txt", "/Users/someone/Library/LaunchAgents/"+label+".plist")), nil
		case scheduler.Unknown:
			return []byte(f.recording("failed-unrelated.txt", plist)), errors.New("exit status 1")
		case scheduler.Missing:
		}
		return []byte(f.recording("missing.txt", plist)), errors.New("exit status 113")
	case len(args) == 2 && args[0] == "bootout":
		label := args[1][strings.LastIndex(args[1], "/")+1:]
		if f.held(label) == scheduler.Missing {
			return []byte("Boot-out failed: 113: Could not find specified service"), errors.New("exit status 113")
		}
		delete(f.state, label)
		return nil, nil
	case len(args) == 3 && args[0] == "bootstrap":
		label := strings.TrimSuffix(filepath.Base(args[2]), ".plist")
		if _, err := os.Stat(args[2]); err != nil || f.held(label) != scheduler.Missing {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		f.state[label], f.plists[label] = scheduler.Loaded, args[2]
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected launchctl %q", args)
}

func (f *fakeLaunchctl) held(label string) scheduler.JobState {
	if state, ok := f.state[label]; ok {
		return state
	}
	return scheduler.Missing
}

func (f *fakeLaunchctl) Put(site scheduler.Site, ref scheduler.Ref, state scheduler.JobState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[string(ref)], f.plists[string(ref)] = state, PlistPath(site, ref)
}

func (f *fakeLaunchctl) Held(_ scheduler.Site, ref scheduler.Ref) scheduler.JobState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held(string(ref))
}

func (f *fakeLaunchctl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// launchd passes the conformance suite over a launchctl that prints what the
// recordings say, and its plists are the golden files in
// testdata/conformance (the shape the characterization of setup pins in
// internal/cli/testdata/scheduler/plists, over the suite's own jobs). It is
// not parallel: one of the suite's checks sets the process's environment.
func TestConformance(t *testing.T) {
	schedulertest.RunConformance(t, schedulertest.Backend{
		New: func(t *testing.T) (scheduler.Scheduler, schedulertest.Manager) {
			t.Helper()
			f := newFakeLaunchctl(t)
			return Scheduler{Run: f.run}, f
		},
		Golden: func(t *testing.T, name string, got []byte) {
			t.Helper()
			golden.Check(t, filepath.Join("testdata", "conformance", name+".plist"), got)
		},
	})
}
