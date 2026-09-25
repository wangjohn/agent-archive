package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
)

// twoInstallations is the account's default installation and a test one
// (AGENT_ARCHIVE_HOME elsewhere) sharing one HOME, so one set of app hook
// files, as `AGENT_ARCHIVE_HOME=/tmp/x agent-archive setup` without a
// sandboxed HOME does.
func twoInstallations(t *testing.T) (primary, secondary Env, userHome string) {
	t.Helper()
	account, userHome := t.TempDir(), t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	primary = setupTestEnv(t, filepath.Join(account, ".local", "share", "agent-archive"), userHome, newFakeKeychain(), now)
	primary.AccountHome = func() (string, error) { return account, nil }
	secondary = setupTestEnv(t, t.TempDir(), userHome, newFakeKeychain(), now)
	secondary.AccountHome = func() (string, error) { return account, nil }
	// One executable for both, so each one's status finds its commands.
	secondary.Executable = primary.Executable
	return primary, secondary, userHome
}

func hookFileSnapshot(t *testing.T, userHome string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for app, path := range legacyHookFiles(userHome) {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out[app] = string(data)
	}
	return out
}

func statusViewOf(t *testing.T, env Env) statusView {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("status exit %d: %s", code, &errOut)
	}
	var view statusView
	must(t, json.Unmarshal(out.Bytes(), &view))
	return view
}

// H-20: a second installation in the same HOME never takes over, changes, or
// removes the first one's hooks, in setup, status, or uninstall, whichever
// was installed first.
func TestSecondInstallationNeverTouchesTheFirstsHooks(t *testing.T) {
	for _, order := range []string{"default first", "test first"} {
		t.Run(order, func(t *testing.T) {
			primary, secondary, userHome := twoInstallations(t)
			first, second := primary, secondary
			if order == "test first" {
				first, second = secondary, primary
			}
			input := s3SetupInput("b", "us-east-1", "p", true, true, true, t.TempDir())
			setupRun(t, first, input, 0)
			before := hookFileSnapshot(t, userHome)
			firstHome, _ := first.home()
			secondHome, _ := second.home()
			executable, _ := first.executable()

			// The second setup refuses, names the first installation and how
			// to resolve it, and changes no hook file.
			output := setupRun(t, second, s3SetupInput("b", "us-east-1", "p", true, true, true, t.TempDir()), 1)
			wantOwner := "the installation in " + firstHome
			if order == "default first" {
				wantOwner = "the default installation in " + local.CanonicalPath(firstHome)
			}
			if !strings.Contains(output, wantOwner) || !strings.Contains(output, "agent-archive uninstall") || !strings.Contains(output, "own HOME") || strings.Count(output, wantOwner) != 6 {
				t.Fatalf("setup output does not name the other installation:\n%s", output)
			}
			if after := hookFileSnapshot(t, userHome); !equalMaps(before, after) {
				t.Fatalf("the refused setup changed hook files")
			}
			view := statusViewOf(t, first)
			for _, app := range view.Apps {
				if app.Hooks != "installed" || len(app.OtherInstallations) != 0 {
					t.Fatalf("first installation's %s: hooks=%s others=%v", app.Name, app.Hooks, app.OtherInstallations)
				}
			}

			// The old bug left both installations' handlers in the files.
			// Each status then names the other, and each uninstall removes only
			// its own.
			for _, app := range allHarnesses {
				changes, err := hooks.Plan(legacyHookFiles(userHome), second.installation(secondHome, userHome).hook(executable), []string{app})
				must(t, err)
				must(t, hooks.Apply(changes))
			}
			view = statusViewOf(t, first)
			for _, app := range view.Apps {
				if app.Hooks != "installed" || len(app.OtherInstallations) != 1 {
					t.Fatalf("first installation's %s beside another: hooks=%s others=%v", app.Name, app.Hooks, app.OtherInstallations)
				}
			}
			if len(view.Warnings) == 0 || !strings.Contains(strings.Join(view.Warnings, "\n"), "another agent-archive installation") {
				t.Fatalf("status warnings: %v", view.Warnings)
			}
			var out, errOut bytes.Buffer
			if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errOut, first); code != 0 {
				t.Fatalf("uninstall exit %d: %s%s", code, &out, &errOut)
			}
			if !strings.Contains(out.String(), "Kept: ") {
				t.Fatalf("uninstall does not say what it kept:\n%s", &out)
			}
			for app, path := range legacyHookFiles(userHome) {
				owner := second.installation(secondHome, userHome).hook(executable)
				if ok, err := hooks.Installed(legacyHookFiles(userHome), owner, app); !ok || err != nil {
					data, _ := os.ReadFile(path)
					t.Fatalf("uninstalling the first installation removed the second's %s hooks (%v):\n%s", app, err, data)
				}
				others, err := hooks.OtherInstallations(legacyHookFiles(userHome), owner, app)
				must(t, err)
				if len(others) != 0 {
					t.Fatalf("the first installation's %s hooks were left: %+v", app, others)
				}
			}
		})
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
