package collector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func linkedSessionEvidenceCount(req state.Request, childID string) int {
	count := 0
	for _, item := range req.HookEvidence {
		if item.Kind != archive.EvidenceKindLinkedSession {
			continue
		}
		if id, _ := item.Payload["archive_session_id"].(string); id == childID {
			count++
		}
	}
	return count
}

// A parent whose own request never clears (its transcript fails the filter, so
// every pass fails before publication) is re-notified about its published
// child on every collector pass. The notification must not accumulate: the
// request file would otherwise grow by one identical evidence item forever.
func TestRepeatedParentLinkNotificationDoesNotGrowTheRequest(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	// The parent's transcript holds no recognized records, so it can never
	// publish and its request is never acknowledged.
	parent := registration(t, writeTranscript(t, t.TempDir(), "unsafe.jsonl", `{"type":"unrecognized_record"}`+"\n"))
	parent.ArchiveSessionID = "parent"
	parent.NativeSessionID = "native-parent"
	child := registration(t, "")
	child.ArchiveSessionID = "child"
	child.NativeSessionID = "native-child"
	child.ParentSessionID = "parent"
	for _, reg := range []archive.SessionRegistration{parent, child} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	filtered, err := archive.CodexAdapter{}.FilterJSONL(strings.NewReader(`{"type":"turn_context","model":"synthetic"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(child, archive.CodexAdapter{}, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := statetest.SavePublished(local, "child", bundle, at, state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemoryStore()
	for pass := 0; pass < 5; pass++ {
		now := at.Add(time.Duration(pass) * time.Hour)
		if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }}); err != nil {
			t.Fatal(err)
		}
		request, found, err := local.LoadRequest("parent")
		if err != nil || !found {
			t.Fatalf("pass %d lost the parent notification: %+v %v", pass, request, err)
		}
		if got := linkedSessionEvidenceCount(request, "child"); got != 1 {
			t.Fatalf("pass %d: parent request carries %d linked_session items, want 1", pass, got)
		}
		if len(request.HookEvidence) != 1 {
			t.Fatalf("pass %d: parent request carries %d evidence items, want 1", pass, len(request.HookEvidence))
		}
	}
}

// Linking a child is a note about another session, not new activity in this
// one. Publishing it must not move the parent's capture time, which is what
// retention measures the parent's lifetime from.
func TestChildLinkDoesNotExtendParentRetentionBasis(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	parent := registration(t, path)
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	first := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return first }}); err != nil {
		t.Fatal(err)
	}
	firstMetadata := fetchMetadata(t, store, "codex", parent.ArchiveSessionID)

	linkedAt := first.Add(30 * time.Minute)
	evidence, err := archive.NewLinkedSessionEvidence("child", archive.LinkedSessionPublished, linkedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(parent.ArchiveSessionID, "subagent-published", linkedAt, evidence); err != nil {
		t.Fatal(err)
	}
	second := first.Add(time.Hour)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return second }}); err != nil {
		t.Fatal(err)
	}
	secondMetadata := fetchMetadata(t, store, "codex", parent.ArchiveSessionID)
	if len(secondMetadata.LinkedSessions) != 1 || secondMetadata.LinkedSessions[0].SessionID != "child" {
		t.Fatalf("child link was not recorded: %#v", secondMetadata.LinkedSessions)
	}
	if !secondMetadata.CapturedAt.Equal(firstMetadata.CapturedAt) {
		t.Fatalf("a link-only update extended the parent's retention basis: %s -> %s", firstMetadata.CapturedAt, secondMetadata.CapturedAt)
	}

	// Real new activity still moves the capture time.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	third := second.Add(time.Hour)
	if err := local.SaveRequest(parent.ArchiveSessionID, "stop", third); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return third }}); err != nil {
		t.Fatal(err)
	}
	thirdMetadata := fetchMetadata(t, store, "codex", parent.ArchiveSessionID)
	if !thirdMetadata.CapturedAt.Equal(third) {
		t.Fatalf("new native evidence did not refresh captured_at: %s", thirdMetadata.CapturedAt)
	}
}

// A parent whose transcript the application deleted is blocked, and blocking
// acknowledges its request. Re-notifying it about a published child would
// write a request and discard it again on every pass for the rest of the
// gap. The link must instead wait quietly and reach the parent's publication
// once the parent's transcript is back.
func TestBlockedParentIsNotRenotifiedUntilItRecovers(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	parentPath := writeTranscript(t, dir, "parent.jsonl", codexTranscript)
	parent := registration(t, parentPath)
	parent.ArchiveSessionID = "parent"
	parent.NativeSessionID = "native-parent"
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	run := func(now time.Time) {
		t.Helper()
		if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }}); err != nil {
			t.Fatal(err)
		}
	}
	run(at)
	if err := os.Remove(parentPath); err != nil {
		t.Fatal(err)
	}

	child := registration(t, filepath.Join(t.TempDir(), "child-not-read.jsonl"))
	child.ArchiveSessionID = "child"
	child.NativeSessionID = "native-child"
	child.ParentSessionID = "parent"
	if err := local.SaveRegistration(child); err != nil {
		t.Fatal(err)
	}
	filtered, err := archive.CodexAdapter{}.FilterJSONL(strings.NewReader(`{"type":"turn_context","model":"synthetic"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(child, archive.CodexAdapter{}, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := statetest.SavePublished(local, "child", bundle, at, state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 3; pass++ {
		run(at.Add(time.Duration(pass) * time.Hour))
		if reason, blocked, err := local.LoadBlocked("parent"); err != nil || !blocked || reason != state.BlockedReasonTranscriptMissing {
			t.Fatalf("pass %d: parent reason=%q blocked=%t err=%v", pass, reason, blocked, err)
		}
		if pass > 1 {
			if _, found, err := local.LoadRequest("parent"); err != nil || found {
				t.Fatalf("pass %d: a blocked parent was re-notified (found=%t err=%v)", pass, found, err)
			}
		}
	}

	// The transcript returns. Pass 4 clears the block; pass 5 announces the
	// link to the parent; pass 6 publishes it, since Run snapshots requests
	// at the start of a pass and so sees a mid-pass notification one pass
	// later (as it always has).
	writeTranscript(t, dir, "parent.jsonl", codexTranscript)
	run(at.Add(4 * time.Hour))
	if _, blocked, err := local.LoadBlocked("parent"); err != nil || blocked {
		t.Fatalf("parent still blocked after its transcript returned: %v", err)
	}
	run(at.Add(5 * time.Hour))
	run(at.Add(6 * time.Hour))
	metadata := fetchMetadata(t, store, "codex", "parent")
	if len(metadata.LinkedSessions) != 1 || metadata.LinkedSessions[0].SessionID != "child" {
		t.Fatalf("child link was not published after the parent recovered: %#v", metadata.LinkedSessions)
	}
}
