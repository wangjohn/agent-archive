package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// fakeLaunchd answers launchctl print, bootstrap, and bootout for one test
// as launchd would: loaded maps a label to the plist launchd loaded it
// from. Nothing here reaches the real launchctl.
type fakeLaunchd struct {
	loaded      map[string]string
	failLoading string // a plist whose bootstrap fails
	calls       []string
}

func (l *fakeLaunchd) run(args ...string) ([]byte, error) {
	l.calls = append(l.calls, strings.Join(args, " "))
	switch args[0] {
	case "print":
		label := filepath.Base(args[1])
		from, ok := l.loaded[label]
		if !ok {
			return []byte("Could not find service \"" + label + "\" in domain for user gui"), errors.New("exit status 113")
		}
		return []byte("\tpath = " + from + "\n\tstate = running\n"), nil
	case "bootstrap":
		if args[2] == l.failLoading {
			return nil, errors.New("exit status 5")
		}
		l.loaded[launchLabel(args[2])] = args[2]
	case "bootout":
		delete(l.loaded, filepath.Base(args[1]))
	}
	return nil, nil
}

// bootouts are the labels launchctl bootout was asked to stop.
func (l *fakeLaunchd) bootouts() []string {
	var labels []string
	for _, call := range l.calls {
		if target, ok := strings.CutPrefix(call, "bootout "); ok {
			labels = append(labels, filepath.Base(target))
		}
	}
	return labels
}

// Regression: PR #57 second review. Setup retired at most one collector an
// earlier release left for this data directory under another label, so
// with two (the default label from before labels were per directory, and a
// label from each spelling setup was ever run with) the rest kept running
// beside the new job. Setup now retires every one, and uninstall removes
// every one, through the real launchctl path: a job is stopped only when
// launchd loaded it from that very plist, so the default-label job a
// sandbox's plist names (the user's real collector) is never booted out,
// and nothing of another directory, or under a label this tool never
// makes, is touched.
func TestEveryEarlierLabelOfTheDirectoryIsRetired(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.JobState, env.LoadLaunchAgent, env.UnloadLaunchAgent = nil, nil, nil
	agents := filepath.Join(userHome, "Library", "LaunchAgents")
	write := func(label, dataHome string) string {
		t.Helper()
		path := filepath.Join(agents, label+".plist")
		plist, err := hooks.LaunchAgent("/opt/old/agent-archive", dataHome, label, nil)
		must(t, err)
		must(t, local.WriteBytes(path, plist))
		return path
	}
	elsewhere := t.TempDir()
	loadedA := write(hooks.CollectorLabel("/old/spelling/a", ""), home)
	unloadedB := write(hooks.CollectorLabel("/old/spelling/b", ""), home)
	// This directory's plist under the default label, but launchd runs the
	// default label from the real account's plist.
	realDefault := write(hooks.LaunchLabel, home)
	otherDirectory := write(hooks.CollectorLabel(elsewhere, ""), elsewhere)
	notOurs := write(hooks.LaunchLabel+".backup", home)
	launchd := &fakeLaunchd{loaded: map[string]string{
		launchLabel(loadedA):        loadedA,
		hooks.LaunchLabel:           "/Users/real/Library/LaunchAgents/" + hooks.LaunchLabel + ".plist",
		launchLabel(otherDirectory): otherDirectory,
	}}
	stubLaunchctl(t, launchd.run)
	own := env.installation(home, userHome).collectorPlist()

	var out, errOut bytes.Buffer
	if code := Run([]string{"setup"}, strings.NewReader(s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir())), &out, &errOut, env); code != 0 {
		t.Fatalf("setup: exit %d\n%s", code, &errOut)
	}
	if got := launchd.bootouts(); !slices.Equal(got, []string{launchLabel(loadedA)}) {
		t.Fatalf("setup booted out %v, want only %s", got, launchLabel(loadedA))
	}
	for _, gone := range []string{loadedA, unloadedB} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s remains after setup (%v)", gone, err)
		}
	}
	for _, kept := range []string{realDefault, otherDirectory, notOurs, own} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s: %v", kept, err)
		}
	}
	if launchd.loaded[launchLabel(own)] != own {
		t.Fatal("the collector was not loaded under the directory's own label")
	}

	// Two more jobs from earlier spellings appear; uninstall stops and
	// removes them with its own, and nothing else.
	loadedC := write(hooks.CollectorLabel("/old/spelling/c", ""), home)
	unloadedD := write(hooks.CollectorLabel("/old/spelling/d", ""), home)
	launchd.loaded[launchLabel(loadedC)] = loadedC
	launchd.calls = nil
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, &errOut)
	}
	got := launchd.bootouts()
	slices.Sort(got)
	want := []string{launchLabel(own), launchLabel(loadedC)}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("uninstall booted out %v, want %v", got, want)
	}
	for _, gone := range []string{own, loadedC, unloadedD} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s remains after uninstall (%v)", gone, err)
		}
	}
	for _, kept := range []string{realDefault, otherDirectory, notOurs} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s: %v", kept, err)
		}
	}
	if !strings.Contains(out.String(), "belongs to another installation") {
		t.Errorf("uninstall did not say it left the default-label job:\n%s", &out)
	}
}

// A setup that retired several earlier-label jobs and then fails puts every
// one of them back as it was, and restarts the ones that were running.
func TestFailedSetupRestoresEveryRetiredJob(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.JobState, env.LoadLaunchAgent, env.UnloadLaunchAgent = nil, nil, nil
	agents := filepath.Join(userHome, "Library", "LaunchAgents")
	plists := map[string][]byte{}
	var paths []string
	for i := range 3 {
		label := hooks.CollectorLabel(fmt.Sprintf("/old/spelling/%d", i), "")
		path := filepath.Join(agents, label+".plist")
		plist, err := hooks.LaunchAgent("/opt/old/agent-archive", home, label, nil)
		must(t, err)
		must(t, local.WriteBytes(path, plist))
		plists[path] = plist
		paths = append(paths, path)
	}
	own := env.installation(home, userHome).collectorPlist()
	launchd := &fakeLaunchd{failLoading: own, loaded: map[string]string{
		launchLabel(paths[0]): paths[0],
		launchLabel(paths[2]): paths[2],
	}}
	stubLaunchctl(t, launchd.run)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup"}, strings.NewReader(s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir())), &out, &errOut, env); code == 0 {
		t.Fatal("setup succeeded although the collector could not start")
	}
	if len(launchd.bootouts()) != 2 {
		t.Fatalf("booted out %v; the test no longer retires the running jobs", launchd.bootouts())
	}
	for path, before := range plists {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, before) {
			t.Errorf("%s not restored (%v)", path, err)
		}
	}
	for _, path := range []string{paths[0], paths[2]} {
		if launchd.loaded[launchLabel(path)] != path {
			t.Errorf("%s was running and was not restarted", path)
		}
	}
	if _, running := launchd.loaded[launchLabel(paths[1])]; running {
		t.Errorf("%s was not running and was started", paths[1])
	}
	if setupjournal.TransactionPending(home) {
		t.Fatalf("the setup journal remains after a complete rollback:\n%s", &errOut)
	}
}
