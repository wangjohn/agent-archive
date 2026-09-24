package state

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// State written before the source reference was recorded carries it in
// cached metadata of any metadata schema version.
func TestOlderStateSourceReferenceIgnoresMetadataSchemaVersion(t *testing.T) {
	ref := archive.SourceReference{Key: "sessions/codex/s/source.x.jsonl.gz", SHA256: strings.Repeat("ab", 32), CompressedBytes: 10}
	metadata, err := json.Marshal(map[string]any{"schema_version": 99, "source_bundle": ref})
	if err != nil {
		t.Fatal(err)
	}
	state := publishedState{Status: CacheStatusPublished, MetadataBytes: metadata}
	if got, found := state.lastPublishedSource(); !found || got != ref {
		t.Fatalf("source = %#v %v", got, found)
	}
	state.MetadataBytes, _ = json.Marshal(map[string]any{"source_bundle": map[string]any{"key": ref.Key, "sha256": "not-a-digest"}})
	if _, found := state.lastPublishedSource(); found {
		t.Fatal("accepted a reference without a SHA-256 digest")
	}
}

// ForgetSession must not go ahead while the session's own subagent candidate
// cannot be read: the candidate would register the session again later.
func TestForgetSessionRefusesWhileItsCandidateIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	store := newTestStore(t)
	if err := store.SaveRegistration(registration(t, "/unused")); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	candidate := SubagentCandidate{ArchiveSessionID: "session-1", NativeSessionID: "native-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "native-parent", ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at}
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.subagentCandidatePath("session-1"), 0); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetSession("session-1", "native-1"); err == nil {
		t.Fatal("forgot a session whose candidate could not be read")
	}
	if _, found, err := store.LoadRegistration("session-1"); err != nil || !found {
		t.Fatalf("registration = %v %v", found, err)
	}
}
