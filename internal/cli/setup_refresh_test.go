package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// refreshRun runs setup --refresh with args, without a terminal, as the
// installer does.
func refreshRun(t *testing.T, env Env, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	env.IsTerminal = func(any) bool { return false }
	var out, errOut bytes.Buffer
	code = Run(append([]string{"setup", "--refresh"}, args...), strings.NewReader(""), &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// recordLaunchd replaces env's scheduler with one that records its calls and
// reports every job in state ("loaded" or "missing"); loading and unloading a
// job change its state, as launchd does.
func recordLaunchd(t *testing.T, env *Env, state string) *fakeScheduler {
	t.Helper()
	sched := newFakeScheduler(t, state)
	env.Scheduler = sched
	return sched
}

// upgradedTo makes env run a new executable, as after an upgrade or a move,
// and returns its path.
func upgradedTo(t *testing.T, env *Env) string {
	t.Helper()
	next := testExecutable(t)
	env.Executable = func() (string, error) { return next, nil }
	return next
}

// tree is every file, directory and link under the roots, by path: a file's
// content hash, "dir", or "link".
func tree(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				out[path] = "link"
			case d.IsDir():
				out[path] = "dir"
			default:
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(data)
				out[path] = hex.EncodeToString(sum[:])
			}
			return nil
		}))
	}
	return out
}

// differences is the paths that were added, removed, or changed between
// two trees, sorted.
func differences(before, after map[string]string) []string {
	var out []string
	for path, sum := range after {
		if before[path] != sum {
			out = append(out, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// An upgrade fixture: a set-up installation (Codex and Claude Code, every
// skill file) whose executable then changes.
type refreshFixture struct {
	home     string
	userHome string
	env      Env
	oldExe   string
	newExe   string
	launchd  *fakeScheduler
}

func newRefreshFixture(t *testing.T, state string) *refreshFixture {
	t.Helper()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	cfg := mustLoadConfig(t, home)
	f := &refreshFixture{home: home, userHome: userHome, env: env, oldExe: cfg.InstalledExecutable}
	f.launchd = recordLaunchd(t, &f.env, state)
	f.newExe = upgradedTo(t, &f.env)
	return f
}

func (f *refreshFixture) plist() string {
	return f.env.installation(f.home, f.userHome).collectorPlist()
}

// wantRunning fails unless every hook, the plist, the skills, and the
// recorded path run exe, and status finds nothing out of date or broken.
func (f *refreshFixture) wantRunning(t *testing.T, exe string) {
	t.Helper()
	cfg := mustLoadConfig(t, f.home)
	if cfg.InstalledExecutable != exe {
		t.Errorf("recorded executable = %s, want %s", cfg.InstalledExecutable, exe)
	}
	in := f.env.installation(f.home, f.userHome)
	for _, app := range cfg.Harnesses {
		if ok, err := hooks.Installed(f.env.installedHookFiles(f.userHome, cfg), in.hook(exe), app); err != nil || !ok {
			t.Errorf("%s hooks do not run %s (%v)", app, exe, err)
		}
	}
	plist, err := os.ReadFile(f.plist())
	must(t, err)
	if program, err := hooks.LaunchAgentProgram(plist); err != nil || program != exe {
		t.Errorf("the plist runs %q (%v), want %s", program, err, exe)
	}
	for _, path := range []string{claudeSkillPath(f.userHome), agentsSkillPath(f.userHome)} {
		if !strings.Contains(readText(t, path), exe) {
			t.Errorf("%s does not run %s", path, exe)
		}
	}
	view := statusViewOf(t, f.env)
	if len(view.AgentSkillsOutOfDate) > 0 {
		t.Errorf("status finds skills out of date: %v", view.AgentSkillsOutOfDate)
	}
	for _, warning := range view.Warnings {
		if strings.Contains(warning, "capture has stopped") {
			t.Errorf("status: %s", warning)
		}
	}
}

// Refresh writes a skill file an earlier release rendered and reports the
// person's own file at another skill path, leaving it alone; the run after
// it finds nothing more to do, and asks launchd nothing.
func TestRefreshReplacesAStaleSkillAndLeavesAForeignOne(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	launchd := recordLaunchd(t, &env, "loaded")
	claude, agents := claudeSkillPath(userHome), agentsSkillPath(userHome)
	current := readText(t, claude)
	older := strings.Replace(current, "Run exactly this command", "Run this command", 1)
	if older == current {
		t.Fatal("the skill has no wording to age")
	}
	must(t, os.WriteFile(claude, []byte(older), 0600))
	must(t, os.WriteFile(agents, []byte("my own version\n"), 0600))

	code, stdout, stderr := refreshRun(t, env)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	want := "refreshed 1 skill file\n" +
		"Left ~/.agents/skills/handoff/SKILL.md as it is: it is not this agent-archive installation's (it lacks the marker line, or names another data directory), so /handoff is not installed there.\n"
	if stdout != want {
		t.Fatalf("output\n%q\nwant\n%q", stdout, want)
	}
	if readText(t, claude) != current {
		t.Error("the stale skill was not replaced")
	}
	if readText(t, agents) != "my own version\n" {
		t.Error("the person's own skill changed")
	}
	if calls := launchd.all(); len(calls) != 0 {
		t.Errorf("refresh asked launchd: %v", calls)
	}
	if setupjournal.TransactionPending(home) {
		t.Error("the journal remains")
	}

	// Idempotent: everything is current now. The person's own file is still
	// named, since refresh would have written it.
	code, stdout, stderr = refreshRun(t, env)
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "nothing to refresh\nLeft ~/.agents/skills/handoff/SKILL.md as it is") {
		t.Fatalf("second run: exit %d\n%s\n%s", code, stdout, stderr)
	}
}

// Every skill in the registry is refreshed together, and removed together by
// the opt-out: an out-of-date agent-archive skill (what install.sh leaves after
// an upgrade) is refreshed as /handoff's is, and --no-skills takes both away.
func TestRefreshRefreshesAndRemovesEverySkill(t *testing.T) {
	t.Parallel()
	_, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	launchd := recordLaunchd(t, &env, "loaded")
	all := append([]string{claudeSkillPath(userHome), agentsSkillPath(userHome)}, archiveSkillPaths(userHome)...)
	current := map[string]string{}
	for _, path := range all {
		current[path] = readText(t, path)
		older := strings.Replace(current[path], "The person", "A person", 1)
		if older == current[path] {
			t.Fatalf("%s has no wording to age", path)
		}
		must(t, os.WriteFile(path, []byte(older), 0600))
	}
	code, stdout, stderr := refreshRun(t, env)
	if code != 0 || stdout != "refreshed 4 skill files\n" || stderr != "" {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	for _, path := range all {
		if readText(t, path) != current[path] {
			t.Errorf("refresh did not restore %s", path)
		}
	}
	if code, stdout, _ := refreshRun(t, env); code != 0 || stdout != "nothing to refresh\n" {
		t.Fatalf("second run: %d %q", code, stdout)
	}
	if calls := launchd.all(); len(calls) != 0 {
		t.Errorf("refresh asked launchd: %v", calls)
	}
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	wantSkillFiles(t, nil, all)
}

// With nothing to change, refresh says so in one line, exits 0, writes no
// journal, and never asks launchd anything.
func TestRefreshOfACurrentInstallationSaysNothingToRefresh(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	launchd := recordLaunchd(t, &env, "loaded")
	before := tree(t, home, userHome)
	for range 2 {
		code, stdout, stderr := refreshRun(t, env)
		if code != 0 || stdout != "nothing to refresh\n" || stderr != "" {
			t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
		}
	}
	if changed := differences(before, tree(t, home, userHome)); len(changed) != 0 {
		t.Errorf("a refresh with nothing to do changed %v", changed)
	}
	if calls := launchd.all(); len(calls) != 0 {
		t.Errorf("refresh asked launchd: %v", calls)
	}
}

// After --no-skills, refresh installs none. A skill file of setup's found
// there anyway is removed, as setup would.
func TestRefreshHonorsTheSkillOptOut(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	claude, agents := claudeSkillPath(userHome), agentsSkillPath(userHome)
	written := readText(t, claude)
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	wantSkillFiles(t, nil, []string{claude, agents})
	launchd := recordLaunchd(t, &env, "loaded")

	code, stdout, stderr := refreshRun(t, env)
	if code != 0 || stdout != "nothing to refresh\n" || stderr != "" {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	wantSkillFiles(t, nil, []string{claude, agents})
	for _, dir := range []string{filepath.Join(userHome, ".claude", "skills"), filepath.Join(userHome, ".agents")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("refresh created %s", dir)
		}
	}
	if !mustLoadConfig(t, home).NoSkills {
		t.Error("the opt-out is gone")
	}

	// A file setup wrote, back again (restored from a backup), goes.
	must(t, os.MkdirAll(filepath.Dir(claude), 0700))
	must(t, os.WriteFile(claude, []byte(written), 0600))
	must(t, os.MkdirAll(filepath.Dir(agents), 0700))
	must(t, os.WriteFile(agents, []byte("my own\n"), 0600))
	code, stdout, stderr = refreshRun(t, env)
	if code != 0 || stdout != "refreshed 1 skill file\n" || stderr != "" {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	wantSkillFiles(t, []string{agents}, []string{claude})
	if readText(t, agents) != "my own\n" {
		t.Error("the person's own skill changed")
	}
	if calls := launchd.all(); len(calls) != 0 {
		t.Errorf("refresh asked launchd: %v", calls)
	}
}

// Hooks that run an executable other than the one now running are pointed
// at it, with the plist, the skills, and the recorded path, in one
// transaction. A loaded job is stopped and loaded again so it runs the new
// plist; status, which called capture stopped, is well again.
func TestRefreshRepairsHooksLeftPointingAtAMovedExecutable(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	must(t, os.Remove(f.oldExe))
	if got := strings.Join(statusViewOf(t, f.env).Warnings, "\n"); !strings.Contains(got, "capture has stopped. Run agent-archive setup --refresh") {
		t.Fatalf("status does not point at refresh:\n%s", got)
	}

	code, stdout, stderr := refreshRun(t, f.env)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	want := "refreshed Codex and Claude Code hooks, the background collector (restarted), and 4 skill files; agent-archive now runs from " + f.newExe + " (it was " + f.oldExe + ")\n" +
		hookNextStep["codex"] + "\n"
	if stdout != want {
		t.Fatalf("output\n%q\nwant\n%q", stdout, want)
	}
	f.wantRunning(t, f.newExe)
	ref := jobRef(f.plist())
	if got := f.launchd.changing(); !reflect.DeepEqual(got, []string{"unload " + string(ref), "load " + string(ref)}) {
		t.Errorf("launchd calls %v", got)
	}
	code, stdout, _ = refreshRun(t, f.env)
	if code != 0 || stdout != "nothing to refresh\n" {
		t.Fatalf("second run: exit %d %q", code, stdout)
	}
}

// Hooks an app lost (an edit that removed them) are put back where setup
// recorded them, with the executable unchanged, and status says how before
// and stops needing to after.
func TestRefreshReinstallsHooksAnAppLost(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	launchd := recordLaunchd(t, &env, "loaded")
	settings := filepath.Join(userHome, ".claude", "settings.json")
	must(t, os.WriteFile(settings, []byte("{\"permissions\":{\"allow\":[\"Read\"]}}\n"), 0600))
	view := statusViewOf(t, env)
	if !strings.Contains(view.Next, "Run agent-archive setup --refresh to reinstall the hooks for Claude Code.") {
		t.Fatalf("status next step: %q", view.Next)
	}

	code, stdout, stderr := refreshRun(t, env)
	if code != 0 || stderr != "" || stdout != "refreshed Claude Code hooks\n" {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	if got := readText(t, settings); !strings.Contains(got, `"permissions"`) || !strings.Contains(got, mustLoadConfig(t, home).InstalledExecutable) {
		t.Fatalf("settings after refresh:\n%s", got)
	}
	if strings.Contains(statusViewOf(t, env).Next, "--refresh") {
		t.Error("status still asks for a refresh")
	}
	if calls := launchd.changing(); len(calls) != 0 {
		t.Errorf("launchd: %v", calls)
	}
}

// A collector whose job is not loaded stays that way: refresh rewrites the
// plist and asks launchd nothing beyond the job's state, and never loads it.
func TestRefreshLeavesAnUnloadedJobUnloaded(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "missing")
	code, stdout, stderr := refreshRun(t, f.env)
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "refreshed Codex and Claude Code hooks, the background collector's plist (its job is not loaded, and was left so), and 4 skill files;") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	f.wantRunning(t, f.newExe)
	if calls := f.launchd.changing(); len(calls) != 0 {
		t.Errorf("refresh started or stopped a job: %v", calls)
	}
}

// A collector plist that is gone is not written again: creating it is
// setup's. The hooks and skills are still refreshed.
func TestRefreshDoesNotCreateAMissingPlist(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "missing")
	must(t, os.Remove(f.plist()))
	code, stdout, stderr := refreshRun(t, f.env)
	if code != 0 || stderr != "" || strings.Contains(stdout, "collector") || !strings.HasPrefix(stdout, "refreshed Codex and Claude Code hooks and 4 skill files;") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(f.plist()); !os.IsNotExist(err) {
		t.Errorf("refresh wrote the plist: %v", err)
	}
	if calls := f.launchd.all(); len(calls) != 0 {
		t.Errorf("launchd: %v", calls)
	}
}

// Only the executable in the LaunchAgent plist changes: the environment
// setup gave it (the AWS files its storage check used) stays as it is, even
// when this shell has none.
func TestRefreshKeepsTheCollectorsEnvironment(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	plistPath := f.plist()
	written, err := hooks.LaunchAgent(f.oldExe, f.home, launchLabel(plistPath), map[string]string{"AWS_CONFIG_FILE": "/aws/config", "PATH": "/opt/bin:/usr/bin"})
	must(t, err)
	must(t, os.WriteFile(plistPath, written, 0600))

	code, _, stderr := refreshRun(t, f.env)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	after, err := os.ReadFile(plistPath)
	must(t, err)
	environment, err := hooks.LaunchAgentEnvironment(after)
	must(t, err)
	if want := map[string]string{"AGENT_ARCHIVE_HOME": f.home, "AWS_CONFIG_FILE": "/aws/config", "PATH": "/opt/bin:/usr/bin"}; !reflect.DeepEqual(environment, want) {
		t.Fatalf("plist environment %v, want %v", environment, want)
	}
	if program, _ := hooks.LaunchAgentProgram(after); program != f.newExe {
		t.Fatalf("the plist runs %s", program)
	}
	want, err := hooks.LaunchAgent(f.newExe, f.home, launchLabel(plistPath), map[string]string{"AWS_CONFIG_FILE": "/aws/config", "PATH": "/opt/bin:/usr/bin"})
	must(t, err)
	if !bytes.Equal(after, want) {
		t.Fatalf("plist:\n%s\nwant\n%s", after, want)
	}
}

// Refresh never points the hooks at a build Go deletes, at the temporary
// folder, at a file that cannot run, or at nothing: it refuses, changing
// nothing, and says so on one line.
func TestRefreshRefusesAnExecutableThatIsNotTheInstalledBinary(t *testing.T) {
	t.Parallel()
	notExecutable := filepath.Join(t.TempDir(), "agent-archive")
	must(t, os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o644))
	for _, tc := range []struct {
		name    string
		exe     func(t *testing.T, f *refreshFixture) (string, error)
		message string
	}{
		{"go run build", func(t *testing.T, f *refreshFixture) (string, error) {
			t.Helper()
			return writeExecutable(t, filepath.Join(t.TempDir(), "go-build3141592", "b001", "exe", "agent-archive")), nil
		}, "is a temporary build (from go run or go test)"},
		{"temporary folder", func(t *testing.T, f *refreshFixture) (string, error) {
			t.Helper()
			temp := t.TempDir()
			f.env.TempDir = func() string { return temp }
			return writeExecutable(t, filepath.Join(temp, "agent-archive")), nil
		}, "is in the temporary folder"},
		{"not executable", func(*testing.T, *refreshFixture) (string, error) { return notExecutable, nil }, "is not executable"},
		{"missing", func(t *testing.T, _ *refreshFixture) (string, error) {
			t.Helper()
			return filepath.Join(t.TempDir(), "gone"), nil
		}, "is missing"},
		{"unknown", func(*testing.T, *refreshFixture) (string, error) { return "", errors.New("no executable") }, "cannot find the running agent-archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRefreshFixture(t, "loaded")
			exe, err := tc.exe(t, f)
			f.env.Executable = func() (string, error) { return exe, err }
			before := tree(t, f.home, f.userHome)
			code, stdout, stderr := refreshRun(t, f.env)
			if code != 1 || stdout != "" || !strings.Contains(stderr, tc.message) || !strings.HasSuffix(stderr, "Nothing was changed\n") || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
			}
			if changed := differences(before, tree(t, f.home, f.userHome)); len(changed) != 0 {
				t.Errorf("a refused refresh changed %v", changed)
			}
			if calls := f.launchd.all(); len(calls) != 0 {
				t.Errorf("launchd: %v", calls)
			}
			if mustLoadConfig(t, f.home).InstalledExecutable != f.oldExe {
				t.Error("the recorded executable changed")
			}
		})
	}
}

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	return path
}

// Refresh refuses, exits 1, and changes nothing (creating no data
// directory either) when there is nothing installed to refresh or setup
// has something to settle first.
func TestRefreshRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, f *refreshFixture)
		message string
	}{
		{"setup never ran", func(t *testing.T, f *refreshFixture) {
			t.Helper()
			must(t, os.RemoveAll(f.home))
			must(t, os.Remove(f.plist()))
		}, "not set up yet; run `agent-archive setup` first"},
		{"setup recovery is pending", func(t *testing.T, f *refreshFixture) {
			t.Helper()
			must(t, os.WriteFile(setupjournal.JournalPath(f.home), []byte("{}"), 0600))
		}, "setup was interrupted and needs recovery"},
		{"uninstalled", func(t *testing.T, f *refreshFixture) {
			t.Helper()
			env := f.env
			env.IsTerminal = func(any) bool { return true }
			var out, errOut bytes.Buffer
			if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
				t.Fatalf("uninstall: %s %s", &out, &errOut)
			}
		}, "integrations are not installed"},
		{"unreadable settings", func(t *testing.T, f *refreshFixture) {
			t.Helper()
			must(t, os.WriteFile(filepath.Join(f.home, "config.json"), []byte("{not json"), 0600))
		}, "cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRefreshFixture(t, "loaded")
			tc.arrange(t, f)
			before := tree(t, f.userHome)
			_, homeErr := os.Stat(f.home)
			homeBefore := map[string]string{}
			if homeErr == nil {
				homeBefore = tree(t, f.home)
			}
			f.launchd.forget()
			code, stdout, stderr := refreshRun(t, f.env)
			if code != 1 || stdout != "" || !strings.Contains(stderr, tc.message) || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
			}
			if changed := differences(before, tree(t, f.userHome)); len(changed) != 0 {
				t.Errorf("a refused refresh changed %v", changed)
			}
			if homeErr == nil {
				if changed := differences(homeBefore, tree(t, f.home)); len(changed) != 0 {
					t.Errorf("a refused refresh changed %v", changed)
				}
			} else if _, err := os.Stat(f.home); !os.IsNotExist(err) {
				t.Errorf("a refused refresh created the data directory: %v", err)
			}
			if calls := f.launchd.all(); len(calls) != 0 {
				t.Errorf("launchd: %v", calls)
			}
		})
	}
}

// An uninstall that finishes between refresh's first look at the settings
// and its locks must not be undone: the settings are read again under the
// locks, and the refresh changes nothing.
func TestRefreshRechecksTheSettingsUnderItsLocks(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	uninstaller := f.env
	uninstaller.IsTerminal = func(any) bool { return true }
	exe := f.env.Executable
	var before map[string]string
	f.env.Executable = func() (string, error) {
		// Refresh reads the running executable after its first look at the
		// settings and before it takes any lock.
		var out, errOut bytes.Buffer
		if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, uninstaller); code != 0 {
			t.Errorf("uninstall: %s %s", &out, &errOut)
		}
		before = tree(t, f.userHome)
		return exe()
	}
	code, stdout, stderr := refreshRun(t, f.env)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "integrations are not installed") {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	if changed := differences(before, tree(t, f.userHome)); len(changed) != 0 {
		t.Errorf("a refused refresh changed %v", changed)
	}
}

// A collector pass, an open setup, or a hook that is writing holds a lock
// refresh needs: it refuses, changing nothing, and says to retry.
func TestRefreshRefusesWhileALockIsHeld(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		lock    func(home string) (func(), error)
		message string
	}{
		{"collector", func(home string) (func(), error) { return lockCollector(home, "collect", time.Now()) }, "holds the collector lock; retry when it finishes"},
		{"setup", func(home string) (func(), error) { return local.NamedLock(home, "setup.lock") }, "another setup is running"},
		{"hooks", func(home string) (func(), error) { return local.NamedLock(home, "hooks.lock") }, "a hook is finishing; retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRefreshFixture(t, "loaded")
			f.env.RefreshCollectorWait = func() time.Duration { return 10 * time.Millisecond }
			unlock, err := tc.lock(f.home)
			must(t, err)
			defer unlock()
			before := tree(t, f.userHome)
			code, stdout, stderr := refreshRun(t, f.env)
			if code != 1 || stdout != "" || !strings.Contains(stderr, tc.message) || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
			}
			if changed := differences(before, tree(t, f.userHome)); len(changed) != 0 {
				t.Errorf("a refused refresh changed %v", changed)
			}
		})
	}
}

// A collector pass that is running when the installer upgrades (the
// background collector starts one every minute) is waited for, not a reason
// to fail: the refresh goes through once the pass releases its lock.
func TestRefreshWaitsForARunningCollectorPass(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	f.env.RefreshCollectorWait = func() time.Duration { return time.Minute }
	unlock, err := lockCollector(f.home, "collect", time.Now())
	must(t, err)
	go func() {
		time.Sleep(50 * time.Millisecond)
		unlock()
	}()
	if code, stdout, stderr := refreshRun(t, f.env); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	f.wantRunning(t, f.newExe)
}

// Once files are changing, a signal (Ctrl-C, a closed terminal, SIGTERM)
// must not stop the refresh halfway, leaving a journal to recover and
// capture stopped: the signals are absorbed from before the journal is
// written until it is gone, and only then. A refresh with nothing to change
// never registers for them.
func TestRefreshAbsorbsSignalsWhileItChangesFiles(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	var mu sync.Mutex
	active, registrations := false, 0
	f.env.Interrupts = func() (<-chan os.Signal, func()) {
		mu.Lock()
		defer mu.Unlock()
		active = true
		registrations++
		return make(chan os.Signal), func() {
			mu.Lock()
			defer mu.Unlock()
			active = false
		}
	}
	var duringLoad, duringUnload bool
	f.launchd.beforeLoad = func(schedulerRef) error {
		mu.Lock()
		duringLoad = active
		mu.Unlock()
		return nil
	}
	f.launchd.beforeUnload = func(schedulerRef) error {
		mu.Lock()
		duringUnload = active
		mu.Unlock()
		return nil
	}
	if code, stdout, stderr := refreshRun(t, f.env); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	mu.Lock()
	if !duringLoad || !duringUnload || registrations != 1 || active {
		t.Errorf("signals absorbed while the job was stopped: %t, started: %t; registered %d times; still registered: %t", duringUnload, duringLoad, registrations, active)
	}
	registrations = 0
	mu.Unlock()
	code, stdout, _ := refreshRun(t, f.env)
	mu.Lock()
	defer mu.Unlock()
	if code != 0 || stdout != "nothing to refresh\n" || registrations != 0 {
		t.Errorf("a refresh with nothing to do: exit %d, %q, registered %d times", code, stdout, registrations)
	}
}

// Another installation's hooks in a file this one refreshes are not
// touched, and nothing is refreshed: the same refusal setup makes.
func TestRefreshRefusesWhenAnotherInstallationsHooksAreInTheWay(t *testing.T) {
	t.Parallel()
	primary, secondary, userHome := twoInstallations(t)
	setupRun(t, primary, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
	setupRun(t, secondary, s3SetupInput("b", "us-east-1", "p", true, false, false, t.TempDir()), 0)
	secondHome, _ := secondary.home()
	// The second installation's saved apps now include the first one's.
	cfg := mustLoadConfig(t, secondHome)
	cfg.Harnesses = []string{"codex", "claude"}
	must(t, config.Save(secondHome, cfg))
	upgradedTo(t, &secondary)
	before := tree(t, userHome, secondHome)

	code, stdout, stderr := refreshRun(t, secondary)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "hooks of another agent-archive installation") || !strings.HasSuffix(stderr, "Nothing was changed\n") || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	if changed := differences(before, tree(t, userHome, secondHome)); len(changed) != 0 {
		t.Errorf("a refused refresh changed %v", changed)
	}
}

// A second installation's refresh changes its own hooks, and neither the
// first one's hooks nor its skill, which the second reports as left alone.
func TestRefreshOfOneInstallationLeavesTheOthersFilesAlone(t *testing.T) {
	t.Parallel()
	primary, secondary, userHome := twoInstallations(t)
	setupRun(t, primary, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
	setupRun(t, secondary, s3SetupInput("b", "us-east-1", "p", true, false, false, t.TempDir()), 0)
	secondHome, _ := secondary.home()
	firstHome, _ := primary.home()
	newExe := upgradedTo(t, &secondary)
	claudeSettings := filepath.Join(userHome, ".claude", "settings.json")
	firstBefore := tree(t, firstHome)
	settingsBefore, skillBefore := readText(t, claudeSettings), readText(t, claudeSkillPath(userHome))

	code, stdout, stderr := refreshRun(t, secondary)
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "refreshed Codex hooks, the background collector (restarted), and 2 skill files;") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if readText(t, claudeSettings) != settingsBefore || readText(t, claudeSkillPath(userHome)) != skillBefore {
		t.Error("the first installation's files changed")
	}
	if changed := differences(firstBefore, tree(t, firstHome)); len(changed) != 0 {
		t.Errorf("the first installation's data directory changed: %v", changed)
	}
	cfg := mustLoadConfig(t, secondHome)
	if ok, err := hooks.Installed(secondary.installedHookFiles(userHome, cfg), secondary.installation(secondHome, userHome).hook(newExe), "codex"); err != nil || !ok {
		t.Errorf("the second installation's Codex hooks do not run %s (%v)", newExe, err)
	}
}

// Every flag but --verbose is a usage error, exit 2 and nothing changed.
func TestRefreshTakesNoOtherFlagThanVerbose(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	before := tree(t, f.home, f.userHome)
	for _, args := range [][]string{
		{"--yes"},
		{"--no-skills"},
		{"--skills"},
		{"--abandon-recovery"},
		{"--provider", "s3"},
		{"--apps", "codex"},
		{"--project", "/tmp"},
		{"--skill-evidence", "none"},
		{"--yes", "--verbose"},
	} {
		code, stdout, stderr := refreshRun(t, f.env, args...)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "agent-archive: setup: --refresh takes no other flag than --verbose, and "+args[0]) || !strings.HasSuffix(stderr, "run agent-archive setup --help\n") || strings.Count(stderr, "\n") != 1 {
			t.Errorf("setup --refresh %v: exit %d\n%q\n%q", args, code, stdout, stderr)
		}
	}
	if code, _, stderr := refreshRun(t, f.env, "--bogus"); code != 2 || !strings.Contains(stderr, "unknown flag --bogus") {
		t.Errorf("an unknown flag: exit %d %q", code, stderr)
	}
	if code, _, stderr := refreshRun(t, f.env, "extra"); code != 2 || !strings.Contains(stderr, `unexpected argument "extra"`) {
		t.Errorf("an argument: exit %d %q", code, stderr)
	}
	if changed := differences(before, tree(t, f.home, f.userHome)); len(changed) != 0 {
		t.Errorf("usage errors changed %v", changed)
	}
	if calls := f.launchd.all(); len(calls) != 0 {
		t.Errorf("launchd: %v", calls)
	}
}

// --verbose lists each file refreshed under the line, and is the one flag
// refresh accepts.
func TestRefreshVerboseListsTheFiles(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "missing")
	code, stdout, stderr := refreshRun(t, f.env, "--verbose")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	for _, want := range []string{"\n  ~/.claude/settings.json\n", "\n  ~/.codex/hooks.json\n", "\n  ~/.claude/skills/handoff/SKILL.md\n", "\n  ~/.agents/skills/handoff/SKILL.md\n", "\n  ~/Library/LaunchAgents/com.agent-archive.collector", "config.json\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, stdout)
		}
	}
}

// A paused installation is refreshed like any other, and stays paused:
// pausing keeps the hooks.
func TestRefreshKeepsAPausedInstallationPaused(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	var out, errOut bytes.Buffer
	if code := Run([]string{"pause"}, nil, &out, &errOut, f.env); code != 0 {
		t.Fatalf("pause: %s %s", &out, &errOut)
	}
	if code, stdout, stderr := refreshRun(t, f.env); code != 0 || stderr != "" || !strings.HasPrefix(stdout, "refreshed ") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if !mustLoadConfig(t, f.home).Paused {
		t.Fatal("refresh resumed a paused installation")
	}
}

// A write that fails partway puts every file back and the job as it was: no
// hook, skill, plist, or recorded path is left changed, no journal remains,
// and the person is told.
func TestRefreshFailureRollsBackEveryFile(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	// The shared skill is written after the hook files and Claude Code's
	// skill, so they are applied when this write fails.
	skillDir := filepath.Dir(agentsSkillPath(f.userHome))
	must(t, os.Chmod(skillDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(skillDir, 0o700) })
	before := tree(t, f.home, f.userHome)

	code, stdout, stderr := refreshRun(t, f.env)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "previous installation restored") || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	must(t, os.Chmod(skillDir, 0o700))
	if changed := differences(before, tree(t, f.home, f.userHome)); len(changed) != 0 {
		t.Errorf("a failed refresh left %v changed", changed)
	}
	if setupjournal.TransactionPending(f.home) {
		t.Error("the journal remains")
	}
	// The job was not asked to stop, since the files failed first... it is
	// stopped before they are written and started again by the rollback.
	if ref, calls := jobRef(f.plist()), f.launchd.changing(); !reflect.DeepEqual(calls, []string{"unload " + string(ref), "load " + string(ref)}) {
		t.Errorf("launchd calls %v", calls)
	}
	// With the file writable again, the refresh goes through.
	if code, stdout, stderr = refreshRun(t, f.env); code != 0 {
		t.Fatalf("retry: exit %d\n%s\n%s", code, stdout, stderr)
	}
	f.wantRunning(t, f.newExe)
}

// A failed write with the job left as it is rolls back without asking
// launchd anything.
func TestRefreshFailureWithoutAJobRestartNeverAsksLaunchd(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "missing")
	skillDir := filepath.Dir(agentsSkillPath(f.userHome))
	must(t, os.Chmod(skillDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(skillDir, 0o700) })
	before := tree(t, f.home, f.userHome)
	if code, _, stderr := refreshRun(t, f.env); code != 1 || !strings.Contains(stderr, "previous installation restored") {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	must(t, os.Chmod(skillDir, 0o700))
	if changed := differences(before, tree(t, f.home, f.userHome)); len(changed) != 0 {
		t.Errorf("a failed refresh left %v changed", changed)
	}
	if calls := f.launchd.changing(); len(calls) != 0 {
		t.Errorf("launchd was asked to change a job: %v", calls)
	}
}

// A job that will not start again with the new plist puts the old files and
// the old job back.
func TestRefreshRollsBackWhenTheRestartedJobFails(t *testing.T) {
	t.Parallel()
	f := newRefreshFixture(t, "loaded")
	f.launchd.beforeLoad = func(schedulerRef) error {
		if len(f.launchd.all()) == 3 { // state, unload, then the new plist's load
			return errors.New("bootstrap failed")
		}
		return nil
	}
	before := tree(t, f.home, f.userHome)
	code, stdout, stderr := refreshRun(t, f.env)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "previous installation restored: start background collector: bootstrap failed") {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	if changed := differences(before, tree(t, f.home, f.userHome)); len(changed) != 0 {
		t.Errorf("a failed refresh left %v changed", changed)
	}
	if state := f.launchd.state(jobRef(f.plist())); state != "loaded" {
		t.Errorf("the job was left %s", state)
	}
}

// A job whose state cannot be read, or that another installation runs,
// stops a refresh that would change the plist, before anything is written.
func TestRefreshRefusesAPlistChangeItCannotSafelyRestart(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"unknown", setupjournal.JobAnotherInstallation} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			f := newRefreshFixture(t, state)
			before := tree(t, f.home, f.userHome)
			code, stdout, stderr := refreshRun(t, f.env)
			if code != 1 || stdout != "" || !strings.HasSuffix(stderr, "Nothing was changed\n") || strings.Count(stderr, "\n") != 1 {
				t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
			}
			if changed := differences(before, tree(t, f.home, f.userHome)); len(changed) != 0 {
				t.Errorf("a refused refresh changed %v", changed)
			}
			if calls := f.launchd.changing(); len(calls) != 0 {
				t.Errorf("launchd: %v", calls)
			}
		})
	}
}

// A refresh changes only hook files, the collector's plist, skill files, and
// the recorded executable in the settings: the whole home directory and the
// data directory, before and after, differ in nothing else (the lock files
// and the journal are scratch that comes and goes).
func TestRefreshChangesOnlyHooksPlistSkillsAndTheRecordedExecutable(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"loaded", "missing"} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			f := newRefreshFixture(t, state)
			// Work in the data directory, and files of the person's own next to
			// everything refresh writes.
			must(t, os.WriteFile(filepath.Join(f.home, "registrations.json"), []byte("[]"), 0600))
			must(t, os.WriteFile(filepath.Join(f.userHome, ".claude", "CLAUDE.md"), []byte("my notes\n"), 0600))
			must(t, os.WriteFile(filepath.Join(f.userHome, ".claude", "skills", "handoff", "notes.md"), []byte("mine\n"), 0600))
			must(t, os.WriteFile(filepath.Join(f.userHome, ".codex", "config.toml"), []byte("model = 'x'\n"), 0600))
			before := tree(t, f.home, f.userHome)
			configBefore := readText(t, filepath.Join(f.home, "config.json"))

			if code, stdout, stderr := refreshRun(t, f.env); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
			}
			after := tree(t, f.home, f.userHome)
			allowed := []string{
				filepath.Join(f.userHome, ".claude", "settings.json"),
				filepath.Join(f.userHome, ".codex", "hooks.json"),
				f.plist(),
				claudeSkillPath(f.userHome),
				agentsSkillPath(f.userHome),
				archiveSkillPaths(f.userHome)[0],
				archiveSkillPaths(f.userHome)[1],
				filepath.Join(f.home, "config.json"),
			}
			scratch := []string{"setup.lock", "hooks.lock", "collector.lock", collectorLockRecordName, "setup-transaction.json"}
			for _, path := range differences(before, after) {
				if !slices.Contains(allowed, path) && !slices.Contains(scratch, filepath.Base(path)) {
					t.Errorf("refresh changed %s", path)
				}
			}
			// The settings differ in the recorded executable alone.
			var was, is map[string]any
			must(t, json.Unmarshal([]byte(configBefore), &was))
			must(t, json.Unmarshal([]byte(readText(t, filepath.Join(f.home, "config.json"))), &is))
			if is["installed_executable"] != f.newExe {
				t.Errorf("installed_executable = %v", is["installed_executable"])
			}
			delete(was, "installed_executable")
			delete(is, "installed_executable")
			if !reflect.DeepEqual(was, is) {
				t.Errorf("the settings changed beyond the recorded executable:\n%v\n%v", was, is)
			}
			for path, sum := range before {
				if strings.HasSuffix(path, "notes.md") || strings.HasSuffix(path, "CLAUDE.md") || strings.HasSuffix(path, "config.toml") || strings.HasSuffix(path, "registrations.json") {
					if after[path] != sum {
						t.Errorf("%s changed", path)
					}
				}
			}
			if calls := f.launchd.changing(); state == "missing" && len(calls) != 0 {
				t.Errorf("launchd: %v", calls)
			}
		})
	}
}

// refresh has no way to reach the real launchctl: every call it makes goes
// through the Env's launchd, and with the job as it is (plist unchanged) it
// makes none, not even a question. The isolation stub would fail the test
// on a real one.
func TestRefreshForAnUnchangedPlistNeverCallsLaunchd(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	launchd := recordLaunchd(t, &env, "loaded")
	// Hooks and a skill are stale; the plist is not.
	claudeSettings := filepath.Join(userHome, ".claude", "settings.json")
	cfg := mustLoadConfig(t, home)
	stale := strings.ReplaceAll(readText(t, claudeSettings), cfg.InstalledExecutable, cfg.InstalledExecutable+"-old")
	must(t, os.WriteFile(claudeSettings, []byte(stale), 0600))
	older := strings.Replace(readText(t, agentsSkillPath(userHome)), "Run exactly this command", "Run this command", 1)
	must(t, os.WriteFile(agentsSkillPath(userHome), []byte(older), 0600))

	code, stdout, stderr := refreshRun(t, env)
	if code != 0 || stderr != "" || stdout != "refreshed Claude Code hooks and 1 skill file\n" {
		t.Fatalf("exit %d\n%q\n%q", code, stdout, stderr)
	}
	if calls := launchd.all(); len(calls) != 0 {
		t.Errorf("launchd: %v", calls)
	}
}

// The hooks in a shell without CLAUDE_CONFIG_DIR or CODEX_HOME are found
// where setup recorded them, not where the current environment points.
func TestRefreshUsesTheHookFilesSetupRecorded(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	claudeDir := filepath.Join(userHome, "cfg", "claude")
	must(t, os.MkdirAll(claudeDir, 0700))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.LookupEnv = func(k string) (string, bool) {
		if k == "CLAUDE_CONFIG_DIR" {
			return claudeDir, true
		}
		return "", false
	}
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
	// The installer's shell has no CLAUDE_CONFIG_DIR.
	env.LookupEnv = func(string) (string, bool) { return "", false }
	newExe := upgradedTo(t, &env)
	recordLaunchd(t, &env, "missing")

	code, stdout, stderr := refreshRun(t, env)
	if code != 0 || stderr != "" || !strings.HasPrefix(stdout, "refreshed Claude Code hooks") {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	cfg := mustLoadConfig(t, home)
	if ok, err := hooks.Installed(env.installedHookFiles(userHome, cfg), env.installation(home, userHome).hook(newExe), "claude"); err != nil || !ok {
		t.Errorf("the hooks in %s do not run %s (%v)", cfg.HookFiles["claude"], newExe, err)
	}
	if _, err := os.Stat(filepath.Join(userHome, ".claude")); !os.IsNotExist(err) {
		t.Errorf("refresh wrote to ~/.claude, which setup did not use: %v", err)
	}
	if !strings.Contains(readText(t, filepath.Join(claudeDir, "skills", "handoff", "SKILL.md")), newExe) {
		t.Error("the skill in Claude Code's configuration directory is not refreshed")
	}
}
