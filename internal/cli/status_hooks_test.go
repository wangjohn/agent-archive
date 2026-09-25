package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestStatusChecksHooksAgainstTheInstalledExecutable(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, t.TempDir()))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstalledExecutable != installed {
		t.Fatalf("setup recorded executable %q, want %q", cfg.InstalledExecutable, installed)
	}

	// status run through a different path (a shim, a symlink, a copy).
	env.Executable = func() (string, error) { return "/usr/local/bin/agent-archive", nil }
	hooksByApp := func() map[string]string {
		t.Helper()
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, app := range view.Apps {
			out[app.Name] = app.Hooks
		}
		return out
	}
	for app, state := range hooksByApp() {
		if state != "installed" {
			t.Fatalf("%s hooks = %q from a different executable path", app, state)
		}
	}

	// A configuration written before the field existed falls back to the
	// running executable, which here is not the installed one.
	cfg.InstalledExecutable = ""
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	for app, state := range hooksByApp() {
		if state != "missing or incomplete" {
			t.Fatalf("fallback: %s hooks = %q", app, state)
		}
	}
	env.Executable = func() (string, error) { return installed, nil }
	for app, state := range hooksByApp() {
		if state != "installed" {
			t.Fatalf("fallback with the installed path: %s hooks = %q", app, state)
		}
	}
}

// Regression: CLI correctness review, 2026-09 (e35b8ac).
func TestStatusExplainsWhyHookTrustIsUnknown(t *testing.T) {
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, t.TempDir()))
	var stdout, stderr bytes.Buffer
	if code := runStatusCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "granted inside the app and is not observable") {
		t.Fatalf("status does not explain unknown trust:\n%s", stdout.String())
	}
	stdout.Reset()
	if code := runStatusCommand([]string{"--json"}, &stdout, &stderr, env); code != 0 || !strings.Contains(stdout.String(), `"trust": "unknown"`) {
		t.Fatalf("the JSON value must stay unknown: code=%d\n%s", code, stdout.String())
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestStatusDetectsPartialHooks(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), 0)
	path := filepath.Join(userHome, ".codex", "hooks.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	delete(root["hooks"].(map[string]any), "SessionStart")
	if err := local.Write(path, root); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil || view.State != "Needs attention" || view.Apps[0].Hooks == "installed" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}
