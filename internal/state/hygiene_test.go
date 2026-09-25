package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestForgetSessionRemovesSubagentCandidateLock(t *testing.T) {
	store := newTestStore(t)
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	candidate := SubagentCandidate{ArchiveSessionID: "session-1", NativeSessionID: "native-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "native-parent", ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at}
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(store.home, subagentLockName("session-1"))
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("candidate lock was not created: %v", err)
	}
	if err := store.ForgetSession("session-1", "native-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate lock leaked after ForgetSession: %v", err)
	}
}

// Quarantined copies of one file are capped at quarantineKeep, newest kept.
func TestQuarantineKeepsOnlyTheNewestCopies(t *testing.T) {
	store := newTestStore(t)
	for range quarantineKeep + 2 {
		if err := os.WriteFile(store.requestPath("orphan"), []byte(`{"archive_session_id":"trunc`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, issues, err := store.ScanRequests(); err != nil || !errors.Is(issues["orphan"], ErrQuarantined) {
			t.Fatalf("issues = %v, err = %v", issues, err)
		}
	}
	if files := store.QuarantinedFiles(); len(files) != quarantineKeep {
		t.Fatalf("quarantined = %v, want %d copies", files, quarantineKeep)
	}
}
