package collector

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRunMaterializesAndPublishesSeparateClaudeSubagent(t *testing.T) {
	t.Parallel()
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := materializeSubagentCandidate(ctx, local, state.SubagentCandidate{ArchiveSessionID: "child"}, Options{Sources: testSources}, stopAt); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled materialization: %v", err)
	}
	if _, found, err := local.LoadRegistration("child"); err != nil || found {
		t.Fatalf("canceled child found=%v err=%v", found, err)
	}
	candidates, issues, err := local.ScanSubagentCandidates()
	if err != nil || len(issues) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates=%v issues=%v err=%v", candidates, issues, err)
	}
	remote := storagetest.NewMemoryStore()
	now := stopAt.Add(time.Minute)
	result, err := Run(context.Background(), local, remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }, AcceptSession: func(archive.SessionRegistration) bool { return true }})
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
	t.Parallel()
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
	if err := materializeSubagentCandidate(context.Background(), local, candidate, Options{Sources: testSources}, candidate.ObservedAt); err == nil {
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

// A failed pass can stop after saving the child registration but before
// saving hook evidence or acknowledging the candidate. The next pass must
// finish that work without changing the child's owner or duplicating evidence.
func TestMaterializeResumesAfterRegistrationWrite(t *testing.T) {
	t.Parallel()
	for _, origin := range []archive.SessionOrigin{archive.SessionOriginHook, archive.SessionOriginImport} {
		t.Run(string(origin), func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			local, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
			observed := start.Add(3 * time.Minute)
			childPath := filepath.Join(home, "child.jsonl")
			if err := os.WriteFile(childPath, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			parent := archive.SessionRegistration{
				ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project",
				Harness: archive.Harness{Name: "claude"}, TranscriptPath: childPath, SessionStartedAt: start,
				RegisteredAt: start, AdmittedAt: observed, Origin: origin, DestinationID: "destination",
			}
			if err := local.SaveRegistration(parent); err != nil {
				t.Fatal(err)
			}
			candidate := state.SubagentCandidate{
				ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1",
				ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project",
				ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1",
				TranscriptPath: childPath, ObservedAt: observed, Origin: origin,
			}
			seed := assembleSubagentRegistration(parent, candidate)
			seed.SessionStartedAt = start.Add(2 * time.Minute)
			if err := local.SaveRegistration(seed); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := local.SaveSubagentCandidate(candidate); err != nil {
					t.Fatal(err)
				}
				if err := materializeSubagentCandidate(context.Background(), local, candidate, Options{Sources: testSources}, candidate.ObservedAt); err != nil {
					t.Fatal(err)
				}
			}
			if candidates, err := local.LoadSubagentCandidates(); err != nil || len(candidates) != 0 {
				t.Fatalf("pending candidates=%v err=%v", candidates, err)
			}
			got, found, err := local.LoadRegistration("child")
			if err != nil || !found || got.ParentSessionID != parent.ArchiveSessionID || got.DestinationID != parent.DestinationID || got.Origin != origin || !got.SessionStartedAt.Equal(seed.SessionStartedAt) {
				t.Fatalf("registration=%+v found=%t err=%v", got, found, err)
			}
			request, found, err := local.LoadRequest("child")
			if err != nil {
				t.Fatal(err)
			}
			if origin == archive.SessionOriginImport {
				if found || got.StartedAtSource != archive.StartedAtSourceTranscript {
					t.Fatalf("import request=%+v found=%t start source=%q", request, found, got.StartedAtSource)
				}
			} else if !found || len(request.HookEvidence) != 1 || request.HookEvidence[0].Kind != archive.EvidenceKindLifecycleHook {
				t.Fatalf("hook request=%+v found=%t", request, found)
			}
		})
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
	t.Parallel()
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
	filtered, err := codex.Filter{}.FilterJSONL(strings.NewReader(`{"type":"turn_context","model":"synthetic"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(child, codex.Filter{}, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := statetest.SavePublished(local, "child", bundle, at, state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}

	store := storagetest.NewMemoryStore()
	for pass := range 5 {
		now := at.Add(time.Duration(pass) * time.Hour)
		if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }}); err != nil {
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
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	parent := registration(t, path)
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	first := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return first }}); err != nil {
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
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return second }}); err != nil {
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
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return third }}); err != nil {
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
	t.Parallel()
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
		if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }}); err != nil {
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
	filtered, err := codex.Filter{}.FilterJSONL(strings.NewReader(`{"type":"turn_context","model":"synthetic"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(child, codex.Filter{}, filtered, at, nil)
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
	t.Parallel()
	at := time.Now().UTC()
	reg := archive.SessionRegistration{ParentSessionID: "parent", ParentNativeSessionID: "native-parent", SubagentID: "agent", SessionStartedAt: at, SubagentObservedAt: at.Add(time.Minute)}
	filtered := archive.FilteredTranscript{NativeStartComplete: true, NativeStartAt: at, NativeEndAt: at, SessionIDs: []string{"native-parent"}}
	if err := validateSubagentTranscript(reg, filtered, at.Add(time.Minute)); err == nil {
		t.Fatal("parent transcript without agent identity accepted as child")
	}
	filtered.AgentIDs = []string{"agent"}
	if err := validateSubagentTranscript(reg, filtered, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	filtered.NativeStartAt = at.Add(-time.Hour)
	if err := validateSubagentTranscript(reg, filtered, at.Add(time.Minute)); err == nil {
		t.Fatal("replacement with old child start accepted")
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestMissingChildTranscriptRemainsRetryable(t *testing.T) {
	t.Parallel()
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
	if err := materializeSubagentCandidate(context.Background(), store, candidate, Options{Sources: testSources}, candidate.ObservedAt); err == nil {
		t.Fatal("expected pending transcript error")
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 {
		t.Fatalf("lost delayed child retry: %+v %v", candidates, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestCollectorRepairsParentLinkAfterNotificationFailure(t *testing.T) {
	t.Parallel()
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
	filtered, err := codex.Filter{}.FilterJSONL(strings.NewReader(`{"type":"turn_context","model":"synthetic"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(child, codex.Filter{}, filtered, at, nil)
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
	_, err = Run(context.Background(), local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return at.Add(time.Minute) }, AcceptSession: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == "child" }})
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
	t.Parallel()
	input := `{"type":"unknown_hidden_record","sessionId":"parent","agentId":"agent","timestamp":"2026-09-21T09:00:00Z","content":"not retained"}` + "\n" + `{"type":"assistant","sessionId":"parent","agentId":"agent","timestamp":"2026-09-21T10:00:00Z","message":{"role":"assistant","content":"visible"}}` + "\n"
	filtered, err := claude.Filter{}.FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ParentSessionID: "parent-archive", ParentNativeSessionID: "parent", SubagentID: "agent", SessionStartedAt: at, SubagentObservedAt: at.Add(time.Minute)}
	if err := validateSubagentTranscript(reg, filtered, at.Add(time.Minute)); err == nil {
		t.Fatal("excluded old native record was ignored for eligibility")
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestLaterChildStopSurvivesEarlierCaptureAcknowledgement(t *testing.T) {
	t.Parallel()
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

// resumedSubagentFixture is a parent session and one Claude Code subagent of
// it, with helpers to write the subagent's records, report its stops, and run
// passes, for the tests of subagents resumed after they stop.
type resumedSubagentFixture struct {
	t         *testing.T
	local     *state.Store
	remote    *storagetest.MemoryStore
	start     time.Time
	childPath string
}

func newResumedSubagentFixture(t *testing.T) *resumedSubagentFixture {
	t.Helper()
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	f := &resumedSubagentFixture{t: t, local: local, remote: storagetest.NewMemoryStore(), start: time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC), childPath: filepath.Join(home, "child.jsonl")}
	parentPath := filepath.Join(home, "parent.jsonl")
	if err := os.WriteFile(parentPath, []byte(`{"type":"assistant","sessionId":"parent-native","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: f.start, RegisteredAt: f.start}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	return f
}

// write appends a child record dated minutes after the parent's start.
func (f *resumedSubagentFixture) write(minutes int) {
	f.t.Helper()
	file, err := os.OpenFile(f.childPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	at := f.start.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
	if _, err := file.WriteString(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"` + at + `","message":{"role":"assistant","content":"child"}}` + "\n"); err != nil {
		f.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatal(err)
	}
}

// stop reports a SubagentStop minutes after the parent's start.
func (f *resumedSubagentFixture) stop(minutes int) {
	f.t.Helper()
	if err := f.local.SaveSubagentCandidate(state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: f.childPath, ObservedAt: f.start.Add(time.Duration(minutes) * time.Minute)}); err != nil {
		f.t.Fatal(err)
	}
}

// run runs a pass minutes after the parent's start and fails on any error.
func (f *resumedSubagentFixture) run(minutes int) Result {
	f.t.Helper()
	now := f.start.Add(time.Duration(minutes) * time.Minute)
	result, err := Run(context.Background(), f.local, f.remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }, AcceptSession: func(archive.SessionRegistration) bool { return true }})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(result.Errors) != 0 {
		f.t.Fatalf("pass at +%dm failed: %v", minutes, result.Errors)
	}
	return result
}

// messages is the child's published message count.
func (f *resumedSubagentFixture) messages() int {
	f.t.Helper()
	metadata := fetchMetadata(f.t, f.remote, "claude", "child")
	if metadata.Counts.Messages == nil {
		f.t.Fatal("child metadata has no message count")
	}
	return *metadata.Counts.Messages
}

func isRunning(result Result) bool {
	return slices.Equal(result.RunningSubagents, []string{"child"}) && !slices.Contains(result.Published, "child")
}

// A subagent resumed after its stop writes records past the bound that stop
// set. It waits, still running, at its published snapshot rather than failing
// every pass; its next stop, or its transcript going quiet when no stop
// comes, publishes the rest.
func TestResumedSubagentWaitsForItsNextStop(t *testing.T) {
	t.Parallel()
	f := newResumedSubagentFixture(t)
	f.write(2)
	f.stop(3)
	if result := f.run(4); !slices.Contains(result.Published, "child") {
		t.Fatalf("first stop result=%#v", result)
	}

	// Resumed: a record after the first stop, and no second stop yet.
	f.write(5)
	result := f.run(10)
	if !isRunning(result) {
		t.Fatalf("running pass result=%#v", result)
	}
	if n := f.messages(); n != 1 {
		t.Fatalf("running subagent published %d messages, want the snapshot of its last stop", n)
	}
	status, err := f.local.LoadStatus()
	if err != nil || status.RunningSubagents != 1 {
		t.Fatalf("status running=%d err=%v", status.RunningSubagents, err)
	}

	f.stop(6)
	if result := f.run(70); len(result.RunningSubagents) != 0 || f.messages() != 2 {
		t.Fatalf("after its next stop: result=%#v messages=%d", result, f.messages())
	}

	// Resumed again, and its session closed before it stopped: once its
	// transcript is quiet for the grace, what it wrote is archived.
	f.write(80)
	if result := f.run(80 + 29); !isRunning(result) {
		t.Fatalf("inside the grace: result=%#v", result)
	}
	if result := f.run(80 + 30); len(result.RunningSubagents) != 0 || f.messages() != 3 {
		t.Fatalf("quiet for the grace: result=%#v messages=%d", result, f.messages())
	}
}

// A subagent resumed before the pass after its stop is registered, not
// rejected, and held to that stop: it publishes at its next stop, or once its
// transcript is quiet.
func TestSubagentResumedBeforeRegistrationWaits(t *testing.T) {
	t.Parallel()
	for _, next := range []string{"stop", "quiet"} {
		t.Run(next, func(t *testing.T) {
			t.Parallel()
			f := newResumedSubagentFixture(t)
			f.write(2)
			f.stop(3)
			f.write(5)
			result := f.run(6)
			if !isRunning(result) || len(result.RejectedSubagents) != 0 {
				t.Fatalf("resumed candidate result=%#v", result)
			}
			if reg, found, err := f.local.LoadRegistration("child"); err != nil || !found || !reg.SubagentObservedAt.Equal(f.start.Add(3*time.Minute)) {
				t.Fatalf("registration=%+v found=%v err=%v", reg, found, err)
			}
			if request, _, err := f.local.LoadRequest("parent"); err != nil || strings.Contains(fmt.Sprint(request.HookEvidence), string(archive.LinkedSessionUnavailable)) {
				t.Fatalf("parent told the link is unavailable: %+v err=%v", request.HookEvidence, err)
			}
			at := 5 + 30
			if next == "stop" {
				f.stop(7)
				at = 8
			}
			if result := f.run(at); len(result.RunningSubagents) != 0 || !slices.Contains(result.Published, "child") || f.messages() != 2 {
				t.Fatalf("after %s: result=%#v", next, result)
			}
		})
	}
}

// A registered subagent that stops again and is resumed before the next pass
// is one running subagent, and that pass records its later stop.
func TestRestoppedRunningSubagentCountsOnceAndKeepsItsStop(t *testing.T) {
	t.Parallel()
	f := newResumedSubagentFixture(t)
	f.write(2)
	f.stop(3)
	f.run(4)
	f.write(5)
	f.stop(6)
	f.write(7)
	result := f.run(8)
	if !isRunning(result) {
		t.Fatalf("result=%#v", result)
	}
	if reg, _, err := f.local.LoadRegistration("child"); err != nil || !reg.SubagentObservedAt.Equal(f.start.Add(6*time.Minute)) {
		t.Fatalf("later stop not recorded: observed=%v err=%v", reg.SubagentObservedAt, err)
	}
	status, err := f.local.LoadStatus()
	if err != nil || status.RunningSubagents != 1 {
		t.Fatalf("status running=%d err=%v", status.RunningSubagents, err)
	}
}

// A record dated beyond the grace ahead of the clock never goes quiet, so it
// is a provenance failure, not a subagent still running: a registered child
// fails the pass, and a candidate is rejected rather than kept for good.
func TestFutureDatedSubagentRecordIsNotRunning(t *testing.T) {
	t.Parallel()
	t.Run("registered", func(t *testing.T) {
		t.Parallel()
		f := newResumedSubagentFixture(t)
		f.write(2)
		f.stop(3)
		f.run(4)
		f.write(60)
		result, err := Run(context.Background(), f.local, f.remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return f.start.Add(10 * time.Minute) }, AcceptSession: func(archive.SessionRegistration) bool { return true }})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.RunningSubagents) != 0 || !errors.Is(result.Errors["child"], errSubagentFutureRecord) {
			t.Fatalf("result=%#v", result)
		}
	})
	t.Run("candidate", func(t *testing.T) {
		t.Parallel()
		f := newResumedSubagentFixture(t)
		f.write(2)
		f.stop(3)
		f.write(60)
		result, err := Run(context.Background(), f.local, f.remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return f.start.Add(10 * time.Minute) }, AcceptSession: func(archive.SessionRegistration) bool { return true }})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.RunningSubagents) != 0 || result.RejectedSubagents["child"] != "subagent_provenance_unavailable" {
			t.Fatalf("result=%#v", result)
		}
		if candidates, err := f.local.LoadSubagentCandidates(); err != nil || len(candidates) != 0 {
			t.Fatalf("candidate kept: %+v err=%v", candidates, err)
		}
	})
}

// This pins the shared assembler contract with a synthetic discovery parent;
// automatic Codex discovery cannot currently produce a child candidate.
func TestMaterializeDiscoveryChildKeepsNativeStartProvenance(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(home, "child.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"synthetic child"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, SessionStartedAt: start, RegisteredAt: start, AdmittedAt: start.Add(time.Minute), Origin: archive.SessionOriginDiscovery, DestinationID: "destination", StartedAtSource: archive.StartedAtSourceTranscript}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: parent.Harness, AgentID: "agent-1", TranscriptPath: path, ObservedAt: start.Add(3 * time.Minute)}
	if err := local.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := materializeSubagentCandidate(context.Background(), local, candidate, Options{Sources: testSources}, candidate.ObservedAt); err != nil {
		t.Fatal(err)
	}
	got, found, err := local.LoadRegistration("child")
	if err != nil || !found || got.StartedAtSource != archive.StartedAtSourceTranscript || got.Origin != parent.Origin || got.DestinationID != parent.DestinationID || !got.AdmittedAt.Equal(parent.AdmittedAt) || !got.SessionStartedAt.Equal(start.Add(2*time.Minute)) {
		t.Fatalf("child provenance=%+v found=%t err=%v", got, found, err)
	}
	var metadata archive.Metadata
	metadata.ApplyRegistrationProvenance(got)
	if metadata.StartedAtSource != archive.StartedAtSourceTranscript || metadata.Origin != archive.SessionOriginDiscovery {
		t.Fatalf("metadata provenance=%+v", metadata)
	}
	// A repeated stop must not rewrite the established admission or start.
	candidate.ObservedAt = candidate.ObservedAt.Add(time.Minute)
	if err := local.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := materializeSubagentCandidate(context.Background(), local, candidate, Options{Sources: testSources}, candidate.ObservedAt); err != nil {
		t.Fatal(err)
	}
	again, _, err := local.LoadRegistration("child")
	if err != nil || again.StartedAtSource != got.StartedAtSource || !again.SessionStartedAt.Equal(got.SessionStartedAt) || !again.AdmittedAt.Equal(got.AdmittedAt) {
		t.Fatalf("repeat changed provenance=%+v err=%v", again, err)
	}
}
