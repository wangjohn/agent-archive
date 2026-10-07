package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
)

func TestCodexExceptionRequiresNamedOtherAppConsentBeforeAdmission(t *testing.T) {
	t.Parallel()
	for _, app := range []string{"claude", "cursor"} {
		t.Run(app, func(t *testing.T) {
			t.Parallel()
			home, userHome, a, b := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			at := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic", "--aws-profile", "profile", "--region", "us-east-1", "--apps", "codex,"+app, "--codex-discovery", "on", "--codex-capture-scope", "all-projects", "--project", a)
			payload := func(id string) map[string]any {
				if app == "cursor" {
					return map[string]any{"hook_event_name": "sessionStart", "conversation_id": id, "session_id": id, "workspace_roots": []any{b}, "transcript_path": nil}
				}
				return map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": id, "cwd": b, "transcript_path": writeTestTranscript(t, "synthetic-claude.jsonl", "")}
			}
			checkAdmission := func(id string, want int) {
				t.Helper()
				must(t, handleTestHookEvent(home, app, payload(id), env.now().Add(time.Minute)))
				regs, err := state.OpenReadOnly(home).LoadRegistrations()
				must(t, err)
				if len(regs) != want {
					t.Fatalf("%s registrations=%+v, want %d", id, regs, want)
				}
				for _, reg := range regs {
					if reg.Harness.Name != app || !mustLoadConfig(t, home).AcceptSession(reg) {
						t.Fatalf("wrong admitted or publication-ineligible session: %+v", reg)
					}
				}
			}
			checkAdmission("before-exception", 0)
			old := mustLoadConfig(t, home)
			next := old
			var out bytes.Buffer
			must(t, promptCodexExceptions(newPrompter(strings.NewReader("include\n"+b+"\n\ndone\n"), &out), &next, userHome))
			if !reflect.DeepEqual(next, old) {
				t.Fatal("default No changed permission")
			}
			if !strings.Contains(out.String(), appName(app)) || !strings.Contains(out.String(), "2) No (default)") || !strings.Contains(out.String(), "Capture permissions are unchanged") {
				t.Fatalf("named/default-No consent missing: %s", &out)
			}
			must(t, applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env))
			checkAdmission("after-no", 0)
			old = mustLoadConfig(t, home)
			next = old
			if err := promptCodexExceptions(newPrompter(strings.NewReader("include\n"+b+"\n"), &out), &next, userHome); err == nil {
				t.Fatal("EOF consent accepted")
			}
			if !reflect.DeepEqual(next, old) {
				t.Fatal("EOF changed permission")
			}
			must(t, promptCodexExceptions(newPrompter(strings.NewReader("include\n"+b+"\ny\ndone\n"), &out), &next, userHome))
			env.Now = func() time.Time { return at.Add(2 * time.Minute) }
			must(t, applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env))
			checkAdmission("after-yes", 1)
			old = mustLoadConfig(t, home)
			next = old
			must(t, promptCodexExceptions(newPrompter(strings.NewReader("exclude\n"+b+"\n\ndone\n"), &out), &next, userHome))
			if !reflect.DeepEqual(next, old) {
				t.Fatal("default No exclusion revoked capture")
			}
			must(t, promptCodexExceptions(newPrompter(strings.NewReader("exclude\n"+b+"\ny\ndone\n"), &out), &next, userHome))
			env.Now = func() time.Time { return at.Add(4 * time.Minute) }
			must(t, applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env))
			regs, err := state.OpenReadOnly(home).LoadRegistrations()
			must(t, err)
			if mustLoadConfig(t, home).AcceptSession(regs[0]) {
				t.Fatal("approved exclusion did not stop other-app publication")
			}
			must(t, handleTestHookEvent(home, app, payload("after-exclude"), at.Add(5*time.Minute)))
			regs, err = state.OpenReadOnly(home).LoadRegistrations()
			must(t, err)
			if len(regs) != 1 {
				t.Fatalf("approved exclusion admitted another session: %+v", regs)
			}
		})
	}
}
