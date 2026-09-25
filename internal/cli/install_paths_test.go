package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
)

// A configuration from before hook_files was recorded had its hooks
// installed at the legacy paths, whatever CLAUDE_CONFIG_DIR says now: status
// checks them there and uninstall removes them from there.
//
// Regression: hook ownership review, 2026-09 (1a9420b).
func TestConfigWithoutRecordedHookFilesUsesTheLegacyPaths(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cfg, _, _ := config.Load(home)
	cfg.HookFiles = nil
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(userHome, "elsewhere")
	must(t, os.MkdirAll(elsewhere, 0700))
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
