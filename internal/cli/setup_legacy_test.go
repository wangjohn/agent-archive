package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/scheduler"
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
			path := filepath.Join(userHome, "Library", "LaunchAgents", setupjournal.LegacyLaunchLabel+".plist")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(legacyPlist), 0600); err != nil {
				t.Fatal(err)
			}
			legacy := jobRef(path)
			sched := fakeSched(env).set(legacy, "loaded")
			sched.beforeLoad = func(ref scheduler.Ref) error {
				if fail && ref != legacy {
					return errors.New("start failed")
				}
				return nil
			}
			want := 0
			if fail {
				want = 1
			}
			setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), want)
			if !slices.Contains(sched.unloaded(), legacy) {
				t.Fatal("legacy job not stopped")
			}
			data, err := os.ReadFile(path)
			if fail {
				if err != nil || string(data) != legacyPlist || sched.state(legacy) != "loaded" {
					t.Fatalf("not restored: %s %v %v", data, err, sched.state(legacy))
				}
			} else if !os.IsNotExist(err) || sched.state(legacy) != "missing" {
				t.Fatalf("not retired: %v %v", err, sched.state(legacy))
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
	path := filepath.Join(userHome, "Library", "LaunchAgents", setupjournal.LegacyLaunchLabel+".plist")
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, []byte(legacyPlist), 0600))
	prototype := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"python old.py hook","statusMessage":"Recording private skill-run evidence"}]}]}}`
	settings := filepath.Join(userHome, ".claude", "settings.json")
	must(t, os.MkdirAll(filepath.Dir(settings), 0700))
	must(t, os.WriteFile(settings, []byte(prototype), 0600))
	sched := fakeSched(env).set(jobRef(path), "loaded")
	sched.beforeUnload = func(ref scheduler.Ref) error {
		t.Errorf("unloaded %s", ref)
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

const legacyPlist = `<?xml version="1.0"?><plist><dict><key>Label</key><string>com.agent-skills.skill-runs-upload</string><key>ProgramArguments</key><array><string>/usr/bin/python3</string><string>/private/runtime/skill_runs.py</string><string>--home</string><string>/private/records</string><string>upload</string></array></dict></plist>`
