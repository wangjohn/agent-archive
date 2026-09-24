package cli

import (
	"bytes"
	"context"
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
	journal := setupJournal{Changes: []hooks.Change{{Path: settings, Before: []byte("{}\n"), After: installed, Existed: true, Mode: 0600}}, Plist: env.installation(home, userHome).collectorPlist()}
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
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); !os.IsNotExist(err) {
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
	if _, err := os.Stat(env.installation(home, userHome).collectorPlist()); !os.IsNotExist(err) {
		t.Fatal("uninstall --yes left the collector")
	}
}

// Each command's own flag set decides its arguments, and every command
// reports a bad one the same way: one line on stderr naming the problem
// and the command's help, exit 2, and never the flag package's usage dump.
func TestCommandsRejectUnknownArguments(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "--jsn"}, "agent-archive: status: unknown flag --jsn; run agent-archive status --help"},
		{[]string{"status", "extra"}, `agent-archive: status: unexpected argument "extra"; run agent-archive status --help`},
		{[]string{"sync", "--json"}, "agent-archive: sync: unknown flag --json; run agent-archive sync --help"},
		{[]string{"pause", "now"}, `agent-archive: pause: unexpected argument "now"; run agent-archive pause --help`},
		{[]string{"resume", "-x"}, "agent-archive: resume: unknown flag --x; run agent-archive resume --help"},
		{[]string{"setup", "--yes"}, "agent-archive: setup: unknown flag --yes; run agent-archive setup --help"},
		{[]string{"uninstall", "--delete-local"}, "agent-archive: uninstall: unknown flag --delete-local; run agent-archive uninstall --help"},
		{[]string{"uninstall", "--yes", "extra"}, `agent-archive: uninstall: unexpected argument "extra"; run agent-archive uninstall --help`},
		{[]string{"list", "--bogus"}, "agent-archive: list: unknown flag --bogus; run agent-archive list --help"},
		{[]string{"list", "--since"}, "agent-archive: list: --since needs a value; run agent-archive list --help"},
		{[]string{"list", "--complete=maybe"}, `agent-archive: list: invalid value "maybe" for --complete: parse error; run agent-archive list --help`},
		{[]string{"show", "--bogus"}, "agent-archive: show: unknown flag --bogus; run agent-archive show --help"},
		{[]string{"show"}, "agent-archive: show: a SESSION_ID is required (see agent-archive list); run agent-archive show --help"},
		{[]string{"show", "a", "b"}, `agent-archive: show: unexpected argument "b"; run agent-archive show --help`},
		{[]string{"handoff", "--latest", "--bogus"}, "agent-archive: handoff: unknown flag --bogus; run agent-archive handoff --help"},
		{[]string{"handoff", "--max-bytes", "x", "--latest"}, `agent-archive: handoff: invalid value "x" for --max-bytes: parse error; run agent-archive handoff --help`},
		{[]string{"feedback", "SESSION", "--bogus"}, "agent-archive: feedback: unknown flag --bogus; run agent-archive feedback --help"},
		{[]string{"backfill", "--bogus"}, "agent-archive: backfill: unknown flag --bogus; run agent-archive backfill --help"},
		{[]string{"backfill", "--since", "yesterday"}, `agent-archive: backfill: --since: "yesterday" is not a date (2026-01-31), an RFC 3339 time, or an age (7d, 12h); run agent-archive backfill --help`},
		{[]string{"backfill", "undo", "--bogus"}, "agent-archive: backfill undo: unknown flag --bogus; run agent-archive backfill undo --help"},
		{[]string{"backfill", "history", "extra"}, `agent-archive: backfill history: unexpected argument "extra"; run agent-archive backfill history --help`},
	} {
		var out, errOut bytes.Buffer
		code := Run(tc.args, nil, &out, &errOut, env)
		if code != 2 || errOut.String() != tc.want+"\n" || out.Len() != 0 {
			t.Errorf("%v: exit %d\nstderr=%q\n  want=%q", tc.args, code, errOut.String(), tc.want+"\n")
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
				dataHome = env.installation(home, userHome).defaultDataHome()
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
			if states[env.installation(home, userHome).collectorPlist()] != "loaded" {
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
				dataHome = env.installation(home, userHome).defaultDataHome()
			}
			old := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
			plist, _ := hooks.LaunchAgent("/opt/old/agent-archive", dataHome, hooks.LaunchLabel)
			if err := local.WriteBytes(old, plist); err != nil {
				t.Fatal(err)
			}
			current := env.installation(home, userHome).collectorPlist()
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

// The default label belongs to the account's own default installation
// only. A sandbox that overrides $HOME (and so moves the data directory with
// it) gets a label of its own, as does AGENT_ARCHIVE_HOME set elsewhere.
func TestOnlyTheAccountsDefaultInstallationGetsTheDefaultLabel(t *testing.T) {
	account, sandbox := t.TempDir(), t.TempDir()
	env := Env{AccountHome: func() (string, error) { return account, nil }}
	accountDefault := filepath.Join(account, ".local", "share", "agent-archive")
	sandboxDefault := filepath.Join(sandbox, ".local", "share", "agent-archive")
	for _, tc := range []struct {
		name, home, userHome string
		isDefault            bool
	}{
		{"the account's own install", accountDefault, account, true},
		{"HOME-only sandbox", sandboxDefault, sandbox, false},
		{"AGENT_ARCHIVE_HOME elsewhere", filepath.Join(account, "other"), account, false},
		{"sandbox HOME pointed at the real data directory", accountDefault, sandbox, true},
	} {
		in := env.installation(tc.home, tc.userHome)
		label := launchLabel(in.collectorPlist())
		if in.isDefault() != tc.isDefault || (label == hooks.LaunchLabel) != tc.isDefault {
			t.Errorf("%s: default=%v label=%s", tc.name, in.isDefault(), label)
		}
		if got := serviceTarget(in.collectorPlist()); got != fmt.Sprintf("gui/%d/%s", os.Getuid(), label) {
			t.Errorf("%s: service target %s", tc.name, got)
		}
	}
	unknown := Env{AccountHome: func() (string, error) { return "", fmt.Errorf("no user database") }}
	if unknown.installation(accountDefault, account).isDefault() {
		t.Error("an unreadable account home made an installation the default one")
	}
}

// A label is not proof of ownership: launchd reports the plist it loaded a
// job from, and a job loaded from any other file is another installation's.
func TestParseJobStateComparesTheLoadedPlist(t *testing.T) {
	ours := filepath.Join(t.TempDir(), "com.agent-archive.collector.plist")
	for _, tc := range []struct {
		output string
		err    error
		want   string
	}{
		{"gui/501/com.agent-archive.collector = {\n\tpath = " + ours + "\n\tstate = running\n}", nil, "running"},
		{"\tstate = waiting\n\tpath = " + ours + "\n", nil, "loaded"},
		{"\tpath = /Users/someone/Library/LaunchAgents/com.agent-archive.collector.plist\n\tstate = running\n", nil, jobAnotherInstallation},
		{"\tstate = running\n", nil, "unknown"},
		{"Could not find service \"x\" in domain for user gui: 501", fmt.Errorf("exit status 113"), "missing"},
		{"boom", fmt.Errorf("exit status 1"), "unknown"},
	} {
		if got := parseJobState(tc.output, tc.err, ours); got != tc.want {
			t.Errorf("%q: %s, want %s", tc.output, got, tc.want)
		}
	}
}

// With the real launchctl path (nothing injected but launchctl itself), a
// sandboxed install whose label launchd has loaded from another plist, the
// user's real collector, neither stops nor replaces it: uninstall leaves it
// running and setup refuses before touching anything.
func TestAnotherInstallationsJobIsNeverStopped(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.JobState, env.LoadLaunchAgent, env.UnloadLaunchAgent = nil, nil, nil
	var calls []string
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "print" {
			return []byte("\tpath = /Users/real/Library/LaunchAgents/x.plist\n\tstate = running\n"), nil
		}
		return nil, nil
	})
	var out, errOut bytes.Buffer
	code := Run([]string{"setup"}, strings.NewReader(s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir())), &out, &errOut, env)
	if code != 1 || !strings.Contains(errOut.String(), "another installation") {
		t.Fatalf("setup: exit %d\n%s", code, &errOut)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "print ") {
			t.Fatalf("setup ran launchctl %s", call)
		}
	}
	if err := unloadLaunchAgent(env.installation(home, userHome).collectorPlist()); err == nil || !strings.Contains(err.Error(), "another installation") {
		t.Fatalf("unload: %v", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "bootout") || strings.HasPrefix(call, "bootstrap") {
			t.Fatalf("launchctl %s", call)
		}
	}
}

// Uninstall leaves another installation's job running and says so.
func TestUninstallLeavesAnotherInstallationsJob(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	env.JobState = func(string) string { return jobAnotherInstallation }
	env.UnloadLaunchAgent = func(p string) error { t.Fatalf("unloaded %s", p); return nil }
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
	if !strings.Contains(out.String(), "another installation") {
		t.Fatalf("not reported:\n%s", &out)
	}
	_ = home
	_ = userHome
}

// stubLaunchctl replaces launchctl for one test.
func stubLaunchctl(t *testing.T, run func(args ...string) ([]byte, error)) {
	t.Helper()
	previous := runLaunchctl
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) { return run(args...) }
	t.Cleanup(func() { runLaunchctl = previous })
}
