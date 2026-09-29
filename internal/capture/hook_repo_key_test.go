package capture

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// registerWithRepoKey starts a Codex session in /work/widget with repoKey as
// the hook's lookup and returns the one registration it made.
func registerWithRepoKey(t *testing.T, repoKey RepoKeyFunc) archive.SessionRegistration {
	t.Helper()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	payload := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
	}
	if err := HandleEventWithRepoKey(home, "codex", payload, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), repoKey); err != nil {
		t.Fatalf("the hook failed: %v", err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v, err = %v; want the session registered", regs, err)
	}
	return regs[0]
}

func TestHookRecordsTheRepoKeyOnANewRegistration(t *testing.T) {
	t.Parallel()
	want := archive.RepoKey("https://example.test/acme/widget.git")
	var asked []string
	reg := registerWithRepoKey(t, func(root string) string {
		asked = append(asked, root)
		return want
	})
	if reg.RepoKey != want {
		t.Errorf("RepoKey = %q, want %q", reg.RepoKey, want)
	}
	if len(asked) != 1 || asked[0] != "/work/widget" {
		t.Errorf("the lookup ran for %v, want once for the project root", asked)
	}
}

func TestHookRegistersWithoutARepoKeyWhenThereIsNone(t *testing.T) {
	t.Parallel()
	for name, repoKey := range map[string]RepoKeyFunc{
		"no lookup":                   nil,
		"a project without a remote":  func(string) string { return "" },
		"a lookup that panics":        func(string) string { panic("git blew up") },
		"a lookup returning a url":    func(string) string { return "https://user:token@example.test/acme/widget.git" },
		"a lookup returning nonsense": func(string) string { return "repo-nothex" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if reg := registerWithRepoKey(t, repoKey); reg.RepoKey != "" {
				t.Errorf("RepoKey = %q, want none", reg.RepoKey)
			}
		})
	}
}

func TestHookPlainHandleEventRegistersWithoutARepoKey(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	payload := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
	}
	if err := HandleEvent(home, "codex", payload, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].RepoKey != "" {
		t.Fatalf("registrations = %#v", regs)
	}
}

func TestHookDoesNotAskForARepoKeyForASessionItDeclines(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	payload := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": "/elsewhere/other", "transcript_path": "/tmp/t.jsonl",
	}
	err := HandleEventWithRepoKey(home, "codex", payload, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), func(string) string {
		t.Error("the lookup ran for a project that is not archived")
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
}
