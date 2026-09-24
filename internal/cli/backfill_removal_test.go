package cli

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
)

// A session retention or undo forgot keeps its removal record, so backfill
// reports it as removed instead of importing its transcript again.
func TestBackfillArchiveStateReportsRemovalRecords(t *testing.T) {
	home := t.TempDir()
	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := store.RecordRemoval("codex", "expired-session", collector.RemovalReasonRetention, at); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRemoval("claude", "undone-session", collector.RemovalReasonUndo, at); err != nil {
		t.Fatal(err)
	}
	state := newArchiveState(home, config.Config{})
	for _, tc := range []struct {
		harness, native string
		want            backfill.SkipReason
	}{
		{"codex", "expired-session", backfill.SkipRemovedByRetention},
		{"claude", "undone-session", backfill.SkipRemovedByUndo},
		{"claude", "never-seen", ""},
		// Records are per app: the same ID under another app is not removed.
		{"claude", "expired-session", ""},
	} {
		got, err := state.Classify(tc.harness, tc.native)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("Classify(%s, %s) = %q, want %q", tc.harness, tc.native, got, tc.want)
		}
	}
}

// Removal records only keep backfill from importing a transcript again.
// Hooks ignore them: a fresh start of a native session undo or retention
// removed registers as any other start.
func TestHookFreshStartIgnoresRemovalRecord(t *testing.T) {
	for _, reason := range []collector.RemovalReason{collector.RemovalReasonUndo, collector.RemovalReasonRetention} {
		t.Run(string(reason), func(t *testing.T) {
			home := t.TempDir()
			setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			store, err := collector.NewLocalStore(home)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordRemoval("codex", "native-1", reason, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			payload := map[string]any{
				"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
				"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
			}
			if err := handleHookEvent(home, "codex", payload, now); err != nil {
				t.Fatal(err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if len(regs) != 1 || regs[0].NativeSessionID != "native-1" || regs[0].Imported() || !regs[0].SessionStartedAt.Equal(now) {
				t.Fatalf("regs=%#v", regs)
			}
		})
	}
}
