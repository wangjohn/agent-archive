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
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRunMaterializesAndPublishesSeparateClaudeSubagent(t *testing.T) {
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parentStart := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	parentPath := filepath.Join(home, "parent.jsonl")
	childPath := filepath.Join(home, "child.jsonl")
	if err := os.WriteFile(parentPath, []byte(`{"type":"assistant","sessionId":"parent-native","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: parentStart, RegisteredAt: parentStart}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	stopAt := parentStart.Add(3 * time.Minute)
	if err := local.SaveSubagentCandidate(state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: childPath, ObservedAt: stopAt}); err != nil {
		t.Fatal(err)
	}
	link, err := archive.NewLinkedSessionEvidence("child", archive.LinkedSessionPending, stopAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest("parent", "subagent-link", stopAt, link); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := stopAt.Add(time.Minute)
	result, err := Run(context.Background(), local, remote, Options{MachineID: "machine", Now: func() time.Time { return now }, AcceptSession: func(archive.SessionRegistration) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || len(result.Published) != 2 {
		t.Fatalf("result=%#v", result)
	}
	child, found, err := local.LoadRegistration("child")
	if err != nil || !found || child.ParentSessionID != "parent" || !child.SessionStartedAt.Equal(parentStart.Add(2*time.Minute)) {
		t.Fatalf("child=%#v found=%v err=%v", child, found, err)
	}
	parentMetadata := fetchMetadata(t, remote, "claude", "parent")
	childMetadata := fetchMetadata(t, remote, "claude", "child")
	if parentMetadata.Counts.Messages == nil || *parentMetadata.Counts.Messages != 1 || childMetadata.Counts.Messages == nil || *childMetadata.Counts.Messages != 1 {
		t.Fatalf("counts parent=%#v child=%#v", parentMetadata.Counts, childMetadata.Counts)
	}
	if childMetadata.ParentSessionID != "parent" {
		t.Fatalf("child parent=%q", childMetadata.ParentSessionID)
	}
}

func TestMaterializeRejectsMismatchedSubagentOwnership(t *testing.T) {
	home := t.TempDir()
	local, _ := state.Open(home)
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(home, "wrong.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"other-parent","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"wrong"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: start, RegisteredAt: start}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "child-native", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: path, ObservedAt: start.Add(3 * time.Minute)}
	if err := local.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := materializeSubagentCandidate(local, candidate, Options{}); err == nil {
		t.Fatal("mismatched transcript was accepted")
	}
	if _, found, _ := local.LoadRegistration("child"); found {
		t.Fatal("mismatched child registration persisted")
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != "parent" {
		t.Fatalf("requests=%#v err=%v", requests, err)
	}
}

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
//
// Regression: pre-release review, carried over from agent-skills (e371b6a).
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

	store := storagetest.NewMemoryStore()
	for pass := range 5 {
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
//
// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestChildLinkDoesNotExtendParentRetentionBasis(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	parent := registration(t, path)
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
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
//
// Regression: pre-release review, carried over from agent-skills (e371b6a).
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
	store := storagetest.NewMemoryStore()
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

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestChildProvenanceRequiresAgentIdentityAndStableStart(t *testing.T) {
	at := time.Now().UTC()
	reg := archive.SessionRegistration{ParentSessionID: "parent", ParentNativeSessionID: "native-parent", SubagentID: "agent", SessionStartedAt: at, SubagentObservedAt: at.Add(time.Minute)}
	filtered := archive.FilteredTranscript{NativeStartComplete: true, NativeStartAt: at, NativeEndAt: at, SessionIDs: []string{"native-parent"}}
	if err := validateSubagentTranscript(reg, filtered); err == nil {
		t.Fatal("parent transcript without agent identity accepted as child")
	}
	filtered.AgentIDs = []string{"agent"}
	if err := validateSubagentTranscript(reg, filtered); err != nil {
		t.Fatal(err)
	}
	filtered.NativeStartAt = at.Add(-time.Hour)
	if err := validateSubagentTranscript(reg, filtered); err == nil {
		t.Fatal("replacement with old child start accepted")
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestMissingChildTranscriptRemainsRetryable(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "native-parent", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "claude"}, SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at.Add(-time.Hour)}
	if err := store.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-child", ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: "agent", TranscriptPath: filepath.Join(home, "not-created-yet.jsonl"), ObservedAt: at}
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := materializeSubagentCandidate(store, candidate, Options{}); err == nil {
		t.Fatal("expected pending transcript error")
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 {
		t.Fatalf("lost delayed child retry: %+v %v", candidates, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestCollectorRepairsParentLinkAfterNotificationFailure(t *testing.T) {
	local := newTestStore(t)
	at := time.Now().UTC()
	parent := registration(t, "")
	parent.ArchiveSessionID = "parent"
	parent.NativeSessionID = "native-parent"
	child := registration(t, "")
	child.ArchiveSessionID = "child"
	child.NativeSessionID = "native-child"
	child.ParentSessionID = "parent"
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
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
	blocked := requestPath(local, "parent")
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := markPublishedSubagent(local, child); err == nil {
		t.Fatal("expected parent notification failure")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	// Child source cannot be republished in this pass. The durable publication
	// must still repair its parent notification before any live transcript read.
	_, err = Run(context.Background(), local, storagetest.NewMemoryStore(), Options{MachineID: "machine", Now: func() time.Time { return at.Add(time.Minute) }, AcceptSession: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == "child" }})
	if err != nil {
		t.Fatal(err)
	}
	request, found, err := local.LoadRequest("parent")
	if err != nil || !found || len(request.HookEvidence) == 0 {
		t.Fatalf("parent notification lost: %+v %v", request, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestHiddenOldRecordCannotMakeResumedChildLookFresh(t *testing.T) {
	input := `{"type":"unknown_hidden_record","sessionId":"parent","agentId":"agent","timestamp":"2026-09-21T09:00:00Z","content":"not retained"}` + "\n" + `{"type":"assistant","sessionId":"parent","agentId":"agent","timestamp":"2026-09-21T10:00:00Z","message":{"role":"assistant","content":"visible"}}` + "\n"
	filtered, err := archive.ClaudeAdapter{}.FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ParentSessionID: "parent-archive", ParentNativeSessionID: "parent", SubagentID: "agent", SessionStartedAt: at, SubagentObservedAt: at.Add(time.Minute)}
	if err := validateSubagentTranscript(reg, filtered); err == nil {
		t.Fatal("excluded old native record was ignored for eligibility")
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestLaterChildStopSurvivesEarlierCaptureAcknowledgement(t *testing.T) {
	local := newTestStore(t)
	at := time.Now().UTC()
	first := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-child", ParentArchiveSessionID: "parent", ParentNativeSessionID: "native-parent", ProjectID: "project", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/synthetic/child.jsonl", ObservedAt: at}
	if err := local.SaveSubagentCandidate(first); err != nil {
		t.Fatal(err)
	}
	newer := first
	newer.ObservedAt = at.Add(time.Second)
	if err := local.SaveSubagentCandidate(newer); err != nil {
		t.Fatal(err)
	}
	if err := local.AcknowledgeSubagentCandidate(first); err != nil {
		t.Fatal(err)
	}
	remaining, err := local.LoadSubagentCandidates()
	if err != nil || len(remaining) != 1 || !remaining[0].ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("new stop lost: %+v %v", remaining, err)
	}
	if err := local.AcknowledgeSubagentCandidate(newer); err != nil {
		t.Fatal(err)
	}
	remaining, err = local.LoadSubagentCandidates()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("processed stop retained: %+v %v", remaining, err)
	}
}
