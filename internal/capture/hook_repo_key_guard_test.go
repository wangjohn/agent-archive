package capture

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

var (
	guardActivated = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	guardNow       = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
)

// keyLookup counts its calls and answers with a fixed key.
type keyLookup struct {
	asked int
	key   string
}

func newKeyLookup() *keyLookup {
	return &keyLookup{key: archive.RepoKey("https://example.test/acme/widget.git")}
}

func (l *keyLookup) lookup(string) string { l.asked++; return l.key }

// changeConfig loads home's configuration, applies change, and saves it.
func changeConfig(t *testing.T, home string, change func(*config.Config)) {
	t.Helper()
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("load config: found=%t err=%v", found, err)
	}
	change(&cfg)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
}

// The lookup runs for a start capture will admit, and for nothing else. Each
// case below deletes one guard's reason to ask, so each fails if that guard
// is removed.
func TestHookNeverAsksForARepoKeyForAStartCaptureWillNotAdmit(t *testing.T) {
	t.Parallel()
	resumed := writeTestTranscript(t, "resumed.jsonl", `{"type":"user"}`)
	for _, tc := range []struct {
		name    string
		harness string
		change  func(*config.Config)
		payload map[string]any
	}{
		{"an excluded project", "codex", func(c *config.Config) { c.Archive.Projects[0].Included = false }, startPayload("/work/widget")},
		{"a paused archive", "codex", func(c *config.Config) { c.Paused = true }, startPayload("/work/widget")},
		{"capture switched off", "codex", func(c *config.Config) { c.Archive.Enabled = false }, startPayload("/work/widget")},
		{"a project that is not configured", "codex", nil, startPayload("/elsewhere/other")},
		{"a project not yet active", "codex", func(c *config.Config) { c.Archive.Projects[0].ActivatedAt = guardNow.Add(time.Hour) }, startPayload("/work/widget")},
		{"a start that is not provably fresh", "codex", nil, map[string]any{
			"hook_event_name": "SessionStart", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": resumed,
		}},
		{"a Claude Code prompt", "claude", nil, map[string]any{"hook_event_name": "UserPromptSubmit", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}},
		{"a stop", "claude", nil, map[string]any{"hook_event_name": "Stop", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}},
		{"a subagent stop", "claude", nil, map[string]any{
			"hook_event_name": "SubagentStop", "source": "startup", "session_id": "native-1", "cwd": "/work/widget", "agent_id": "agent-1", "agent_transcript_path": "/never/written.jsonl",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			setUpTestConfig(t, home, "/work/widget", guardActivated)
			if tc.change != nil {
				changeConfig(t, home, tc.change)
			}
			l := newKeyLookup()
			// Events that need a registration to succeed may return an error; only
			// whether git was asked matters here.
			_ = HandleEvent(home, tc.harness, tc.payload, guardNow, WithRepoKey(l.lookup))
			if l.asked != 0 {
				t.Errorf("the lookup ran %d times for %s, want never", l.asked, tc.name)
			}
		})
	}
}

// Cursor registers on a conversation's first prompt, not on a start event, so
// that turn start asks, once; later prompts of the registered chat do not.
func TestHookAsksOnceForACursorConversationsFirstPrompt(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", guardActivated)
	l := newKeyLookup()
	for range 3 {
		payload := cursorDesktopPayload("beforeSubmitPrompt", "conversation-1", "/work/widget", nil)
		if err := HandleEvent(home, "cursor", payload, guardNow, WithRepoKey(l.lookup)); err != nil {
			t.Fatal(err)
		}
	}
	if l.asked != 1 {
		t.Errorf("the lookup ran %d times for one Cursor conversation, want 1", l.asked)
	}
	if reg := onlyCursorRegistration(t, home); reg.RepoKey != l.key {
		t.Errorf("RepoKey = %q, want %q", reg.RepoKey, l.key)
	}
}

// An index entry with no registration behind it (a backfill that stopped
// between assigning the ID and registering, or retention that forgot the
// session) is treated as never seen by the registration, so it gets a key.
func TestHookStillAsksWhenTheIndexHasAnEntryButNoRegistration(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", guardActivated)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-1"}); err != nil {
		t.Fatal(err)
	}
	l := newKeyLookup()
	if err := HandleEvent(home, "codex", startPayload("/work/widget"), guardNow, WithRepoKey(l.lookup)); err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v, err = %v", regs, err)
	}
	if l.asked != 1 || regs[0].RepoKey != l.key {
		t.Errorf("lookup ran %d times and RepoKey = %q, want once and %q", l.asked, regs[0].RepoKey, l.key)
	}
}
