package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// A configuration from before hook_files was recorded had its hooks
// installed at the legacy paths, whatever CLAUDE_CONFIG_DIR says now: status
// checks them there and uninstall removes them from there.
func TestConfigWithoutRecordedHookFilesUsesTheLegacyPaths(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cfg, _, _ := config.Load(home)
	cfg.HookFiles = nil
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(userHome, "elsewhere")
	os.MkdirAll(elsewhere, 0700)
	env.LookupEnv = func(k string) (string, bool) {
		if k == "CLAUDE_CONFIG_DIR" {
			return elsewhere, true
		}
		return "", false
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].Hooks != "installed" {
		t.Fatalf("hooks %s", view.Apps[0].Hooks)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(userHome, ".claude", "settings.json")); strings.Contains(string(b), hooks.Owner) {
		t.Fatalf("hooks left at the legacy path:\n%s", b)
	}
}

// Uninstall also removes hooks an earlier release left at the legacy path
// when the recorded path is elsewhere.
func TestUninstallCleansTheLegacyPathToo(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	claudeDir := filepath.Join(userHome, "cfg")
	os.MkdirAll(claudeDir, 0700)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env.LookupEnv = func(k string) (string, bool) { return claudeDir, k == "CLAUDE_CONFIG_DIR" }
	setupRun(t, env, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
	legacy, err := hooks.Plan(legacyHookFiles(userHome), env.installation(home, userHome).hook("/opt/old/agent-archive"), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Apply(legacy); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("exit %d\n%s", code, &errOut)
	}
	for _, path := range []string{filepath.Join(claudeDir, "settings.json"), legacy[0].Path} {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), hooks.Owner) {
			t.Fatalf("hooks left in %s", path)
		}
	}
}

// Rerunning setup from a shell with a different CLAUDE_CONFIG_DIR moves the
// hooks; the review names the files and says so before anything changes.
func TestSetupReviewWarnsWhenHookFilesMove(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	elsewhere := filepath.Join(userHome, "elsewhere")
	os.MkdirAll(elsewhere, 0700)
	env.LookupEnv = func(k string) (string, bool) { return elsewhere, k == "CLAUDE_CONFIG_DIR" }
	// Change retention (menu choice 3), keep 90 days, then cancel at review.
	var out, errOut bytes.Buffer
	Run([]string{"setup"}, strings.NewReader("3\n90\n3\n"), &out, &errOut, env)
	output := out.String()
	if !strings.Contains(output, "Claude Code hooks: "+filepath.Join(elsewhere, "settings.json")) ||
		!strings.Contains(output, "hooks move here from "+filepath.Join(userHome, ".claude", "settings.json")) {
		t.Fatalf("no warning:\n%s%s", output, &errOut)
	}
	_ = home
}

// A retired collector's plist edited since the interrupted setup blocks
// recovery before anything is touched, naming the way out.
func TestRecoveryStopsOnAnEditedRetiredJob(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	old := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	plist, _ := hooks.LaunchAgent("/opt/old/agent-archive", home, hooks.LaunchLabel)
	if err := local.WriteBytes(old, append(plist, []byte("<!-- edited -->")...)); err != nil {
		t.Fatal(err)
	}
	journal := setupJournal{
		Relabeled: &legacyJob{Change: hooks.Change{Path: old, Before: plist, Existed: true, Mode: 0644}, WasLoaded: true},
		Plist:     env.installation(home, userHome).collectorPlist(),
	}
	if err := local.Write(journalPath(home), journal); err != nil {
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
