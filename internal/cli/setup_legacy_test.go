package cli

import (
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

func TestSetupMigratesLegacyJobAndRestoresOnFailure(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[fail], func(t *testing.T) {
			t.Parallel()
			// The prototype's job is retired by the account's default
			// installation (see TestTestInstallationLeavesPrototypeAlone).
			account, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			home := filepath.Join(account, ".local", "share", "agent-archive")
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			env.AccountHome = func() (string, error) { return account, nil }
			path := filepath.Join(userHome, "Library", "LaunchAgents", legacyLaunchLabel+".plist")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(legacyPlist), 0600); err != nil {
				t.Fatal(err)
			}
			states := map[string]string{path: "loaded"}
			unloaded := false
			env.JobState = func(p string) string {
				if s := states[p]; s != "" {
					return s
				}
				return "missing"
			}
			env.UnloadLaunchAgent = func(p string) error {
				states[p] = "missing"
				if p == path {
					unloaded = true
				}
				return nil
			}
			env.LoadLaunchAgent = func(p string) error {
				if fail && p != path {
					return errors.New("start failed")
				}
				states[p] = "loaded"
				return nil
			}
			want := 0
			if fail {
				want = 1
			}
			setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), want)
			if !unloaded {
				t.Fatal("legacy job not stopped")
			}
			data, err := os.ReadFile(path)
			if fail {
				if err != nil || string(data) != legacyPlist || states[path] != "loaded" {
					t.Fatalf("not restored: %s %v %v", data, err, states)
				}
			} else if !os.IsNotExist(err) || states[path] != "missing" {
				t.Fatalf("not retired: %v %v", err, states)
			}
		})
	}
}

// A second or test installation (here: AGENT_ARCHIVE_HOME elsewhere, in the
// same HOME) leaves the prototype's job and hooks alone: they are the
// account's, and the default installation retires them.
func TestTestInstallationLeavesPrototypeAlone(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	path := filepath.Join(userHome, "Library", "LaunchAgents", legacyLaunchLabel+".plist")
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, []byte(legacyPlist), 0600))
	prototype := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"python old.py hook","statusMessage":"Recording private skill-run evidence"}]}]}}`
	settings := filepath.Join(userHome, ".claude", "settings.json")
	must(t, os.MkdirAll(filepath.Dir(settings), 0700))
	must(t, os.WriteFile(settings, []byte(prototype), 0600))
	env.JobState = func(p string) string {
		if p == path {
			return "loaded"
		}
		return "missing"
	}
	env.UnloadLaunchAgent = func(p string) error {
		t.Errorf("unloaded %s", p)
		return nil
	}
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", false, true, false, project), 0)
	if data, err := os.ReadFile(path); err != nil || string(data) != legacyPlist {
		t.Fatalf("prototype job changed: %v", err)
	}
	if data, _ := os.ReadFile(settings); !strings.Contains(string(data), "old.py") {
		t.Fatalf("prototype hook removed:\n%s", data)
	}
}
