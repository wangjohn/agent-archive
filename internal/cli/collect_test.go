package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
)

// A scheduled pass that meets a corrupt state file still publishes the rest,
// names the quarantined file in status, and keeps collector-error.log (which
// launchd appends to and never rotates) bounded.
func TestCollectPassReportsQuarantinedStateAndTrimsErrorLog(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "registrations", "broken.json"), []byte(`{"archive_session_id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	errorLog := filepath.Join(home, "collector-error.log")
	if err := os.WriteFile(errorLog, bytes.Repeat([]byte("agent-archive: collect: old failure\n"), (errorLogMaxBytes/36)+1), 0o600); err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, home, now)
	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SessionIssues["broken"] != "local_state_unreadable" || strings.Join(status.QuarantinedFiles, ",") != "registrations/broken.json.corrupt" {
		t.Fatalf("status = %#v", status)
	}
	if status.LastPublishedAt.IsZero() {
		t.Fatal("the readable session was not published")
	}
	if info, err := os.Stat(errorLog); err != nil || info.Size() > errorLogKeepBytes {
		t.Fatalf("collector-error.log was not trimmed: %v %v", info.Size(), err)
	}
}
