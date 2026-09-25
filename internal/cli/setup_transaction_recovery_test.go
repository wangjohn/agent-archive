package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// U-22: when launchctl fails while an interrupted setup is being recovered,
// rerunning setup alone stops at the same place, so setup advertises
// --abandon-recovery, as it does for a file changed outside setup. The same
// holds for a recovery record that cannot be read at all, which
// --abandon-recovery then moves aside.
func TestRecoveryBlockedByLaunchctlAdvertisesAbandon(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"restart", "stop"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			plist := env.installation(home, userHome).collectorPlist()
			must(t, local.Write(setupjournal.JournalPath(home), setupjournal.Journal{Plist: plist, WasLoaded: true}))
			state := "missing"
			if failure == "stop" {
				state = "loaded"
			}
			env.JobState = func(string) string { return state }
			env.LoadLaunchAgent = func(string) error { return errors.New("Bootstrap failed: 5: Input/output error") }
			env.UnloadLaunchAgent = func(string) error { return errors.New("Boot-out failed: 5: Input/output error") }
			output := setupRun(t, env, "", 1)
			if !strings.Contains(output, "agent-archive setup --abandon-recovery") || !strings.Contains(output, "launchctl could not") || !strings.Contains(output, setupjournal.JournalPath(home)) {
				t.Fatalf("output:\n%s", output)
			}
		})
	}
}

func TestUnreadableRecoveryRecordHasAWayOut(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	must(t, os.WriteFile(setupjournal.JournalPath(home), []byte(`{"changes":[`), 0o600))
	output := setupRun(t, env, "", 1)
	if !strings.Contains(output, "--abandon-recovery") || !strings.Contains(output, setupjournal.JournalPath(home)) {
		t.Fatalf("setup:\n%s", output)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "--abandon-recovery"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "moved to") {
		t.Fatalf("abandon: exit %d\n%s%s", code, &out, &errOut)
	}
	if setupjournal.TransactionPending(home) {
		t.Fatal("the record is still there")
	}
	if aside, _ := filepath.Glob(setupjournal.JournalPath(home) + ".*" + movedAsideSuffix); len(aside) != 1 {
		t.Fatalf("moved aside: %v", aside)
	}
}

// interruptedSetupWithExternalEdit is the stuck state from the review: a
// setup crashed with settings.json in its journal, and Claude Code then
// rewrote the file, so recovery must not restore it.
func interruptedSetupWithExternalEdit(t *testing.T) (home, userHome, settings string, env Env) {
	t.Helper()
	home, userHome, env = installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	settings = filepath.Join(userHome, ".claude", "settings.json")
	installed, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	journal := setupjournal.Journal{Changes: []hooks.Change{{Path: settings, Before: []byte("{}\n"), After: installed, Existed: true, Mode: 0600}}, Plist: env.installation(home, userHome).collectorPlist()}
	if err := local.Write(setupjournal.JournalPath(home), journal); err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(installed, []byte("{"), []byte("{\n  \"theme\": \"dark\","), 1)
	if err := os.WriteFile(settings, edited, 0600); err != nil {
		t.Fatal(err)
	}
	return home, userHome, settings, env
}

// Every command that refuses to run during an interrupted setup names the
// journal and both ways out, and setup --abandon-recovery gets the user
// unstuck without touching the edited file.
func TestInterruptedSetupHasAWayOut(t *testing.T) {
	t.Parallel()
	home, _, settings, env := interruptedSetupWithExternalEdit(t)
	edited, _ := os.ReadFile(settings)
	run := func(input string, args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Run(args, strings.NewReader(input), &out, &errOut, env)
		return code, out.String() + errOut.String()
	}
	for _, args := range [][]string{{"setup"}, {"uninstall"}, {"pause"}} {
		code, output := run("y\n", args...)
		if code != 1 || !strings.Contains(output, setupjournal.JournalPath(home)) || !strings.Contains(output, "agent-archive setup --abandon-recovery") {
			t.Fatalf("%v: exit %d, output:\n%s", args, code, output)
		}
	}
	if b, _ := os.ReadFile(settings); !bytes.Equal(b, edited) {
		t.Fatal("recovery overwrote the edited settings file")
	}
	code, output := run("", "setup", "--abandon-recovery")
	if code != 0 || !strings.Contains(output, settings) {
		t.Fatalf("abandon: exit %d\n%s", code, output)
	}
	if setupjournal.TransactionPending(home) {
		t.Fatal("the journal is still there")
	}
	if b, _ := os.ReadFile(settings); !bytes.Equal(b, edited) {
		t.Fatal("abandoning recovery changed the settings file")
	}
	if code, output := run("", "pause"); code != 0 {
		t.Fatalf("pause after abandoning: exit %d\n%s", code, output)
	}
	if code, output := run("", "setup", "--abandon-recovery"); code != 0 || !strings.Contains(output, "No interrupted setup") {
		t.Fatalf("second abandon: exit %d\n%s", code, output)
	}
}

// A retired collector's plist edited since the interrupted setup blocks
// recovery before anything is touched, naming the way out.
//
// Regression: hook ownership review, 2026-09 (1a9420b).
func TestRecoveryStopsOnAnEditedRetiredJob(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	old := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	plist, _ := hooks.LaunchAgent("/opt/old/agent-archive", home, hooks.LaunchLabel, nil)
	if err := local.WriteBytes(old, append(plist, []byte("<!-- edited -->")...)); err != nil {
		t.Fatal(err)
	}
	journal := setupjournal.Journal{
		Relabeled: &setupjournal.LegacyJob{Change: hooks.Change{Path: old, Before: plist, Existed: true, Mode: 0644}, WasLoaded: true},
		Plist:     env.installation(home, userHome).collectorPlist(),
	}
	if err := local.Write(setupjournal.JournalPath(home), journal); err != nil {
		t.Fatal(err)
	}
	env.UnloadLaunchAgent = func(p string) error { t.Fatalf("unloaded %s", p); return nil }
	env.LoadLaunchAgent = func(p string) error { t.Fatalf("loaded %s", p); return nil }
	var out, errOut bytes.Buffer
	code := Run([]string{"setup"}, strings.NewReader(""), &out, &errOut, env)
	if code != 1 || !strings.Contains(errOut.String(), old) || !strings.Contains(errOut.String(), "--abandon-recovery") || strings.Contains(errOut.String(), "legacy upload job") {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
	out.Reset()
	if code := Run([]string{"setup", "--abandon-recovery"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "background collector may be stopped") {
		t.Fatalf("abandon: exit %d\n%s", code, &out)
	}
}
