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
