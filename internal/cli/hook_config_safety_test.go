package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

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
	journal := setupJournal{Changes: []hooks.Change{{Path: settings, Before: []byte("{}\n"), After: installed, Existed: true, Mode: 0600}}, Plist: collectorPlist(home, userHome)}
	if err := local.Write(journalPath(home), journal); err != nil {
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
	home, _, settings, env := interruptedSetupWithExternalEdit(t)
	edited, _ := os.ReadFile(settings)
	run := func(input string, args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Run(args, strings.NewReader(input), &out, &errOut, env)
		return code, out.String() + errOut.String()
	}
	for _, args := range [][]string{{"setup"}, {"uninstall"}, {"pause"}} {
		code, output := run("y\n", args...)
		if code != 1 || !strings.Contains(output, journalPath(home)) || !strings.Contains(output, "agent-archive setup --abandon-recovery") {
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
	if transactionPending(home) {
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

// Uninstall only needs the hook files of the apps setup installed. An
// unparsable file of an app that was never selected is reported and left
// alone instead of blocking the collector's removal.
func TestUninstallSkipsAnUnparsableFileOfAnUnselectedApp(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cursor := filepath.Join(userHome, ".cursor", "hooks.json")
	os.MkdirAll(filepath.Dir(cursor), 0700)
	broken := []byte("{\"version\":1,\"hooks\":{},}\n")
	os.WriteFile(cursor, broken, 0600)
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, &out, &errOut)
	}
	if !strings.Contains(out.String(), "Skipped "+cursor) {
		t.Fatalf("the skipped file was not reported:\n%s", &out)
	}
	if b, _ := os.ReadFile(cursor); !bytes.Equal(b, broken) {
		t.Fatal("the unparsable file was changed")
	}
	if _, err := os.Stat(collectorPlist(home, userHome)); !os.IsNotExist(err) {
		t.Fatal("the collector was not removed")
	}
	// The same file blocks uninstall once setup did install Cursor hooks.
	home, userHome, env = installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, false, true, t.TempDir()))
	cursor = filepath.Join(userHome, ".cursor", "hooks.json")
	os.WriteFile(cursor, broken, 0600)
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), cursor) {
		t.Fatalf("exit %d\n%s%s", code, &out, &errOut)
	}
}

// A hook file edited while uninstall runs is reported as such, with uninstall
// (not setup) as the command to rerun.
func TestUninstallConcurrentEditNamesUninstall(t *testing.T) {
	_, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	settings := filepath.Join(userHome, ".claude", "settings.json")
	jobState := env.JobState
	env.JobState = func(p string) string {
		// Runs after uninstall planned its changes: an editor saves the file.
		b, _ := os.ReadFile(settings)
		os.WriteFile(settings, append(b, ' '), 0600)
		return jobState(p)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env)
	if code != 1 || !strings.Contains(errOut.String(), settings) || !strings.Contains(errOut.String(), "rerun agent-archive uninstall") || strings.Contains(errOut.String(), "retry setup") {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
}

// Without a terminal, setup and uninstall stop before asking anything, with
// one line saying why; uninstall --yes works without one.
func TestSetupAndUninstallNeedATerminal(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	env.IsTerminal = func(any) bool { return false }
	for _, cmd := range []string{"setup", "uninstall"} {
		var out, errOut bytes.Buffer
		code := Run([]string{cmd}, strings.NewReader("y\n"), &out, &errOut, env)
		if code != 1 || out.Len() != 0 || strings.Count(errOut.String(), "\n") != 1 || !strings.Contains(errOut.String(), "terminal") {
			t.Fatalf("%s: exit %d stdout=%q stderr=%q", cmd, code, &out, &errOut)
		}
	}
	if cfg, _, _ := config.Load(home); !cfg.Archive.Enabled {
		t.Fatal("refusing changed the configuration")
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall --yes: exit %d\n%s", code, &errOut)
	}
	if _, err := os.Stat(collectorPlist(home, userHome)); !os.IsNotExist(err) {
		t.Fatal("uninstall --yes left the collector")
	}
}

// Each command's own flag set decides its arguments.
func TestCommandsRejectUnknownArguments(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	for _, args := range [][]string{{"status", "--jsn"}, {"status", "extra"}, {"sync", "--json"}, {"pause", "now"}, {"resume", "-x"}, {"setup", "--yes"}, {"uninstall", "--delete-local"}, {"uninstall", "--yes", "extra"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 || !strings.Contains(errOut.String(), "unexpected arguments") {
			t.Errorf("%v: exit %d stderr=%q", args, code, &errOut)
		}
	}
}

// With AGENT_ARCHIVE_HOME set elsewhere, setup installs hooks that carry it
// and a collector under a label of its own, into the files the apps read
// under CLAUDE_CONFIG_DIR and CODEX_HOME; status and uninstall find them
// from a shell without those variables.
func TestRelocatedInstallationStaysSelfContained(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	claudeDir, codexDir := filepath.Join(userHome, "cfg", "claude"), filepath.Join(userHome, "cfg", "codex")
	os.MkdirAll(claudeDir, 0700)
	os.MkdirAll(codexDir, 0700)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	vars := map[string]string{"CLAUDE_CONFIG_DIR": claudeDir, "CODEX_HOME": codexDir}
	env.LookupEnv = func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	var loaded []string
	env.LoadLaunchAgent = func(p string) error { loaded = append(loaded, p); return nil }
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", true, true, false, t.TempDir()), 0)

	if len(loaded) != 1 || filepath.Base(loaded[0]) == hooks.LaunchLabel+".plist" {
		t.Fatalf("a relocated data directory loaded %v", loaded)
	}
	for _, path := range []string{filepath.Join(claudeDir, "settings.json"), filepath.Join(codexDir, "hooks.json")} {
		b, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(b), "AGENT_ARCHIVE_HOME='"+home+"'") {
			t.Fatalf("%s: %v\n%s", path, err, b)
		}
	}
	for _, path := range []string{filepath.Join(userHome, ".claude"), filepath.Join(userHome, ".codex")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("hooks were written under %s", path)
		}
	}
	vars = nil
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range view.Apps {
		if app.Hooks != "installed" {
			t.Fatalf("%s hooks: %s", app.Name, app.Hooks)
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall: %s", &errOut)
	}
	for _, path := range []string{filepath.Join(claudeDir, "settings.json"), filepath.Join(codexDir, "hooks.json")} {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), hooks.Owner) {
			t.Fatalf("uninstall left hooks in %s", path)
		}
	}
}

// An installation from before labels were derived from the data directory
// runs its collector under the default label. Setup moves it to the
// directory's own label, and never touches a default-label job that belongs
// to another data directory.
func TestSetupMovesARelocatedCollectorOffTheDefaultLabel(t *testing.T) {
	for _, owner := range []string{"this", "another"} {
		t.Run(owner, func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			dataHome := home
			if owner == "another" {
				dataHome = defaultDataHome(userHome)
			}
			old := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
			plist, _ := hooks.LaunchAgent("/opt/old/agent-archive", dataHome, hooks.LaunchLabel)
			if err := local.WriteBytes(old, plist); err != nil {
				t.Fatal(err)
			}
			states := map[string]string{old: "loaded"}
			var unloaded []string
			env.JobState = func(p string) string { return states[p] }
			env.LoadLaunchAgent = func(p string) error { states[p] = "loaded"; return nil }
			env.UnloadLaunchAgent = func(p string) error { unloaded = append(unloaded, p); states[p] = "missing"; return nil }
			setupRun(t, env, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
			_, err := os.Stat(old)
			if owner == "this" && (!os.IsNotExist(err) || len(unloaded) != 1 || unloaded[0] != old) {
				t.Fatalf("the old default-label job was not retired: unloaded %v, stat %v", unloaded, err)
			}
			if owner == "another" && (err != nil || len(unloaded) != 0) {
				t.Fatalf("another directory's job was touched: unloaded %v, stat %v", unloaded, err)
			}
			if states[collectorPlist(home, userHome)] != "loaded" {
				t.Fatal("the collector was not loaded under its own label")
			}
		})
	}
}

// Uninstall stops and removes the collector of its own data directory,
// including one an earlier release left under the default label, and never
// the default-label job of another directory: a sandboxed test install must
// not reach the user's real collector.
func TestUninstallTouchesOnlyThisDirectorysCollector(t *testing.T) {
	for _, owner := range []string{"this", "another"} {
		t.Run(owner, func(t *testing.T) {
			home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
			dataHome := home
			if owner == "another" {
				dataHome = defaultDataHome(userHome)
			}
			old := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
			plist, _ := hooks.LaunchAgent("/opt/old/agent-archive", dataHome, hooks.LaunchLabel)
			if err := local.WriteBytes(old, plist); err != nil {
				t.Fatal(err)
			}
			current := collectorPlist(home, userHome)
			states := map[string]string{old: "running", current: "loaded"}
			var unloaded []string
			env.JobState = func(p string) string { return states[p] }
			env.UnloadLaunchAgent = func(p string) error { unloaded = append(unloaded, p); states[p] = "missing"; return nil }
			var out, errOut bytes.Buffer
			if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
				t.Fatalf("exit %d\n%s", code, &errOut)
			}
			_, err := os.Stat(old)
			want := []string{current}
			if owner == "this" {
				want = append(want, old)
				if !os.IsNotExist(err) {
					t.Fatal("the old default-label plist for this directory remains")
				}
			} else if err != nil {
				t.Fatal("another directory's plist was removed")
			}
			if strings.Join(unloaded, ",") != strings.Join(want, ",") {
				t.Fatalf("unloaded %v, want %v", unloaded, want)
			}
		})
	}
}

// launchctl is asked about, and told to stop, the job by its service target,
// which works whether or not the plist is still on disk.
func TestLaunchdServiceTargetUsesTheLabel(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	plist := collectorPlist(home, userHome)
	want := fmt.Sprintf("gui/%d/%s", os.Getuid(), hooks.CollectorLabel(canonicalPath(home), defaultDataHome(userHome)))
	if got := serviceTarget(plist); got != want || strings.Contains(got, ".plist") {
		t.Fatalf("service target %q, want %q", got, want)
	}
	if got := collectorPlist(defaultDataHome(userHome), userHome); filepath.Base(got) != hooks.LaunchLabel+".plist" {
		t.Fatalf("the default data directory's collector moved to %s", got)
	}
}
