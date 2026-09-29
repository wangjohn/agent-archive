package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// subagentGraceFixture registers a parent session and leaves a candidate for
// its subagent as a SubagentStop hook would, observed at the returned time,
// with a transcript path nothing has written yet.
func subagentGraceFixture(t *testing.T, origin archive.SessionOrigin) (*state.Store, time.Time, string) {
	t.Helper()
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parentStart := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	parentPath := filepath.Join(home, "parent.jsonl")
	if err := os.WriteFile(parentPath, []byte(`{"type":"assistant","sessionId":"parent-native","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: parentStart, RegisteredAt: parentStart}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	stopAt := parentStart.Add(3 * time.Minute)
	childPath := filepath.Join(home, "child.jsonl")
	if err := local.SaveSubagentCandidate(state.SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: childPath, ObservedAt: stopAt, Origin: origin}); err != nil {
		t.Fatal(err)
	}
	return local, stopAt, childPath
}

func writeChildTranscript(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runPassAt(t *testing.T, local *state.Store, remote storage.ObjectStore, at time.Time) Result {
	t.Helper()
	result, err := Run(context.Background(), local, remote, Options{MachineID: "machine", Now: func() time.Time { return at }, AcceptSession: func(archive.SessionRegistration) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func pendingCandidates(t *testing.T, local *state.Store) int {
	t.Helper()
	candidates, err := local.LoadSubagentCandidates()
	if err != nil {
		t.Fatal(err)
	}
	return len(candidates)
}

// Claude Code fires SubagentStop for a background agent with a transcript
// path it never writes. The candidate waits out the grace period without
// failing a pass, then is rejected and its parent told the link is
// unavailable, instead of failing every pass for good.
func TestPhantomSubagentWaitsThenIsRejected(t *testing.T) {
	t.Parallel()
	local, stopAt, _ := subagentGraceFixture(t, archive.SessionOriginHook)
	remote := storagetest.NewMemoryStore()

	result := runPassAt(t, local, remote, stopAt.Add(time.Minute))
	if len(result.Errors) != 0 || !slices.Equal(result.WaitingSubagents, []string{"child"}) {
		t.Fatalf("errors=%v waiting=%v", result.Errors, result.WaitingSubagents)
	}
	if n := pendingCandidates(t, local); n != 1 {
		t.Fatalf("candidates=%d, want the waiting one kept", n)
	}
	if status, err := local.LoadStatus(); err != nil || status.WaitingSubagents != 1 || status.LastError != "" || len(status.SessionIssues) != 0 {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	result = runPassAt(t, local, remote, stopAt.Add(subagentTranscriptGrace))
	if len(result.Errors) != 0 || len(result.WaitingSubagents) != 0 || result.RejectedSubagents["child"] != "subagent_transcript_never_written" {
		t.Fatalf("errors=%v waiting=%v rejected=%v", result.Errors, result.WaitingSubagents, result.RejectedSubagents)
	}
	if n := pendingCandidates(t, local); n != 0 {
		t.Fatalf("candidates=%d, want the phantom acknowledged", n)
	}
	if _, found, _ := local.LoadRegistration("child"); found {
		t.Fatal("a subagent without a transcript was registered")
	}
	if status, err := local.LoadStatus(); err != nil || status.WaitingSubagents != 0 || status.LastError != "" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	metadata := fetchMetadata(t, remote, "claude", "parent")
	if len(metadata.LinkedSessions) != 1 || metadata.LinkedSessions[0].SessionID != "child" || metadata.LinkedSessions[0].Status != archive.LinkedSessionUnavailable {
		t.Fatalf("parent links=%#v", metadata.LinkedSessions)
	}
}

// The grace ends exactly subagentTranscriptGrace after the stop, and the
// rejection tells the parent why.
func TestPhantomSubagentRejectionIsRecordedOnTheParent(t *testing.T) {
	t.Parallel()
	local, stopAt, _ := subagentGraceFixture(t, archive.SessionOriginHook)
	outcome := materializeSubagentCandidates(local, Options{}, stopAt.Add(subagentTranscriptGrace-time.Second))
	if len(outcome.errors) != 0 || len(outcome.rejected) != 0 || !slices.Equal(outcome.waiting, []string{"child"}) {
		t.Fatalf("just inside the grace: %+v", outcome)
	}
	outcome = materializeSubagentCandidates(local, Options{}, stopAt.Add(subagentTranscriptGrace))
	if len(outcome.errors) != 0 || len(outcome.waiting) != 0 || len(outcome.rejected) != 1 || outcome.rejected["child"] != "subagent_transcript_never_written" {
		t.Fatalf("at the end of the grace: %+v", outcome)
	}
	request, found, err := local.LoadRequest("parent")
	if err != nil || !found || !slices.Contains(request.Reasons, "subagent_transcript_never_written") || linkedSessionEvidenceCount(request, "child") != 1 {
		t.Fatalf("parent request=%+v found=%t err=%v", request, found, err)
	}
}

// A transcript written late, but inside the grace, is registered and
// published as if it had been there at the stop.
func TestLateSubagentTranscriptIsPublished(t *testing.T) {
	t.Parallel()
	local, stopAt, childPath := subagentGraceFixture(t, archive.SessionOriginHook)
	remote := storagetest.NewMemoryStore()
	if result := runPassAt(t, local, remote, stopAt.Add(time.Minute)); len(result.WaitingSubagents) != 1 {
		t.Fatalf("waiting=%v", result.WaitingSubagents)
	}
	writeChildTranscript(t, childPath)
	result := runPassAt(t, local, remote, stopAt.Add(20*time.Minute))
	if len(result.Errors) != 0 || len(result.WaitingSubagents) != 0 || !slices.Contains(result.Published, "child") {
		t.Fatalf("result=%+v", result)
	}
	if n := pendingCandidates(t, local); n != 0 {
		t.Fatalf("candidates=%d", n)
	}
}

// An empty transcript waits like a missing one, and registers once it holds
// a native record.
func TestEmptySubagentTranscriptWaitsUntilFilled(t *testing.T) {
	t.Parallel()
	local, stopAt, childPath := subagentGraceFixture(t, archive.SessionOriginHook)
	remote := storagetest.NewMemoryStore()
	if err := os.WriteFile(childPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if result := runPassAt(t, local, remote, stopAt.Add(time.Minute)); len(result.Errors) != 0 || len(result.WaitingSubagents) != 1 {
		t.Fatalf("errors=%v waiting=%v", result.Errors, result.WaitingSubagents)
	}
	writeChildTranscript(t, childPath)
	result := runPassAt(t, local, remote, stopAt.Add(5*time.Minute))
	if len(result.Errors) != 0 || len(result.WaitingSubagents) != 0 || !slices.Contains(result.Published, "child") {
		t.Fatalf("result=%+v", result)
	}
}

// A transcript with lines but no recognized record yet waits like an empty
// one: it registers once a recognized record appears, and is rejected as
// never written if none does within the grace.
func TestUnrecognizedSubagentTranscriptWaits(t *testing.T) {
	t.Parallel()
	for _, filled := range []bool{true, false} {
		t.Run(map[bool]string{true: "filled", false: "never"}[filled], func(t *testing.T) {
			t.Parallel()
			local, stopAt, childPath := subagentGraceFixture(t, archive.SessionOriginHook)
			remote := storagetest.NewMemoryStore()
			if err := os.WriteFile(childPath, []byte(`{"type":"unrecognized_record"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if result := runPassAt(t, local, remote, stopAt.Add(time.Minute)); len(result.Errors) != 0 || len(result.WaitingSubagents) != 1 {
				t.Fatalf("errors=%v waiting=%v rejected=%v", result.Errors, result.WaitingSubagents, result.RejectedSubagents)
			}
			if filled {
				writeChildTranscript(t, childPath)
				result := runPassAt(t, local, remote, stopAt.Add(5*time.Minute))
				if len(result.Errors) != 0 || !slices.Contains(result.Published, "child") {
					t.Fatalf("result=%+v", result)
				}
				return
			}
			result := runPassAt(t, local, remote, stopAt.Add(subagentTranscriptGrace))
			if len(result.Errors) != 0 || result.RejectedSubagents["child"] != "subagent_transcript_never_written" {
				t.Fatalf("errors=%v rejected=%v", result.Errors, result.RejectedSubagents)
			}
		})
	}
}

// Each rejection code is either a decision that loses nothing, counted
// quietly, or a lost subagent, reported as a failure.
func TestSubagentRejectionCodesAreClassified(t *testing.T) {
	t.Parallel()
	codes := map[string]bool{
		"subagent_transcript_never_written":     true,
		"subagent_transcript_unavailable":       true,
		"subagent_parent_ownership_unavailable": true,
		"subagent_start_ineligible":             true,
		"subagent_format_unavailable":           false,
		"subagent_transcript_unreadable":        false,
		"subagent_provenance_unavailable":       false,
		"subagent_registration_conflict":        false,
	}
	for code, expected := range codes {
		if expectedSubagentRejections[code] != expected {
			t.Errorf("%s expected=%t, want %t", code, expectedSubagentRejections[code], expected)
		}
	}
	for code := range expectedSubagentRejections {
		if _, known := codes[code]; !known {
			t.Errorf("%s is expected but not classified here", code)
		}
	}
}

// A stop observed further in the future than the grace (the clock has
// jumped back since) is rejected rather than kept waiting that much longer.
func TestFutureSubagentStopIsRejected(t *testing.T) {
	t.Parallel()
	local, stopAt, _ := subagentGraceFixture(t, archive.SessionOriginHook)
	outcome := materializeSubagentCandidates(local, Options{}, stopAt.Add(-subagentTranscriptGrace))
	if len(outcome.errors) != 0 || len(outcome.rejected) != 0 || len(outcome.waiting) != 1 {
		t.Fatalf("a grace ahead: %+v", outcome)
	}
	outcome = materializeSubagentCandidates(local, Options{}, stopAt.Add(-subagentTranscriptGrace-time.Second))
	if len(outcome.errors) != 0 || len(outcome.waiting) != 0 || outcome.rejected["child"] != "subagent_transcript_never_written" {
		t.Fatalf("more than a grace ahead: %+v", outcome)
	}
	if n := pendingCandidates(t, local); n != 0 {
		t.Fatalf("candidates=%d", n)
	}
}

// A transcript that exists but cannot be read (here a directory stands at
// its path) is a subagent lost, not one still to be written: it is rejected
// at once, and the pass reports it as a failure.
func TestUnreadableSubagentTranscriptIsRejectedAsAFailure(t *testing.T) {
	t.Parallel()
	local, stopAt, childPath := subagentGraceFixture(t, archive.SessionOriginHook)
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	result := runPassAt(t, local, storagetest.NewMemoryStore(), stopAt.Add(time.Minute))
	if result.Errors["child"] == nil || len(result.WaitingSubagents) != 0 || result.RejectedSubagents["child"] != "subagent_transcript_unreadable" {
		t.Fatalf("errors=%v waiting=%v rejected=%v", result.Errors, result.WaitingSubagents, result.RejectedSubagents)
	}
	if !errors.Is(result.Errors["child"], ErrSubagentNotCaptured) || !errors.Is(result.Errors["child"], ErrSubagentCandidate) {
		t.Fatalf("error %v does not match ErrSubagentNotCaptured", result.Errors["child"])
	}
	if n := pendingCandidates(t, local); n != 0 {
		t.Fatalf("candidates=%d, want the rejected one acknowledged", n)
	}
	if status, err := local.LoadStatus(); err != nil || status.LastError == "" {
		t.Fatalf("status=%+v err=%v, want the loss recorded", status, err)
	}
}

// A subagent backfill found without a transcript is history that will not be
// written, so it is rejected on the first pass rather than waited for.
func TestImportedSubagentWithoutTranscriptIsRejectedAtOnce(t *testing.T) {
	t.Parallel()
	local, stopAt, _ := subagentGraceFixture(t, archive.SessionOriginImport)
	outcome := materializeSubagentCandidates(local, Options{}, stopAt)
	if len(outcome.errors) != 0 || len(outcome.waiting) != 0 || len(outcome.rejected) != 1 || outcome.rejected["child"] != "subagent_transcript_unavailable" {
		t.Fatalf("outcome=%+v", outcome)
	}
	request, found, err := local.LoadRequest("parent")
	if err != nil || !found || !slices.Contains(request.Reasons, "subagent_transcript_unavailable") {
		t.Fatalf("parent request=%+v found=%t err=%v", request, found, err)
	}
	if gaps := captureGaps(request); len(gaps) != 0 {
		t.Fatalf("an imported subagent left a capture gap: %+v", gaps)
	}
}

// typeSubagentCandidate gives the fixture's candidate agentType, as a later
// SubagentStop naming it would.
func typeSubagentCandidate(t *testing.T, local *state.Store, agentType string) {
	t.Helper()
	candidates, err := local.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	candidate := candidates[0]
	candidate.AgentType = agentType
	if err := local.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
}

// captureGaps returns the capture gap evidence a request holds.
func captureGaps(request state.Request) []archive.SupplementalEvidence {
	var gaps []archive.SupplementalEvidence
	for _, item := range request.HookEvidence {
		if item.Kind == archive.EvidenceKindCaptureGap {
			gaps = append(gaps, item)
		}
	}
	return gaps
}

// neverWrittenGaps returns the never-written capture gaps a request holds.
func neverWrittenGaps(request state.Request) []archive.SupplementalEvidence {
	var gaps []archive.SupplementalEvidence
	for _, item := range captureGaps(request) {
		if item.Payload["code"] == subagentNeverWritten {
			gaps = append(gaps, item)
		}
	}
	return gaps
}

// A subagent whose transcript was never written leaves its parent a capture
// gap saying why it is missing, naming its type when the hook reported a
// valid one, observed at the stop like the link.
func TestNeverWrittenSubagentLeavesItsParentACaptureGap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		agentType string
		detail    string
	}{
		{"typed", "Explore", "Claude Code reported a subagent (type Explore) but never wrote its transcript"},
		{"untyped", "", "Claude Code reported a subagent but never wrote its transcript"},
		// A candidate file edited by hand, or written by another version, is
		// sanitized again before its type is published.
		{"invalid type", "Explore now", "Claude Code reported a subagent but never wrote its transcript"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, stopAt, _ := subagentGraceFixture(t, archive.SessionOriginHook)
			if tc.agentType != "" {
				typeSubagentCandidate(t, local, tc.agentType)
			}
			outcome := materializeSubagentCandidates(local, Options{}, stopAt.Add(subagentTranscriptGrace))
			if len(outcome.errors) != 0 || outcome.rejected["child"] != subagentNeverWritten {
				t.Fatalf("outcome=%+v", outcome)
			}
			request, found, err := local.LoadRequest("parent")
			if err != nil || !found {
				t.Fatalf("found=%t err=%v", found, err)
			}
			gaps := neverWrittenGaps(request)
			if len(gaps) != 1 || gaps[0].Payload["detail"] != tc.detail || gaps[0].Provenance != subagentExpiryProvenance || !gaps[0].ObservedAt.Equal(stopAt) {
				t.Fatalf("gaps=%+v, want one with detail %q", gaps, tc.detail)
			}
			wantType := archive.SanitizeSubagentType(tc.agentType)
			if len(outcome.expired) != 1 || outcome.expired[0].ArchiveSessionID != "child" || outcome.expired[0].AgentType != wantType || !outcome.expired[0].ExpiredAt.Equal(stopAt.Add(subagentTranscriptGrace)) {
				t.Fatalf("expired=%+v, want the child with type %q", outcome.expired, wantType)
			}
		})
	}
}

// A subagent rejected for any other reason leaves no never-written gap and
// is not listed as expired.
func TestOnlyNeverWrittenSubagentsLeaveTheGap(t *testing.T) {
	t.Parallel()
	local, stopAt, childPath := subagentGraceFixture(t, archive.SessionOriginHook)
	typeSubagentCandidate(t, local, "Explore")
	if err := os.Mkdir(childPath, 0o700); err != nil {
		t.Fatal(err)
	}
	outcome := materializeSubagentCandidates(local, Options{}, stopAt.Add(time.Minute))
	if outcome.rejected["child"] != "subagent_transcript_unreadable" || len(outcome.expired) != 0 {
		t.Fatalf("outcome=%+v", outcome)
	}
	request, _, err := local.LoadRequest("parent")
	if err != nil {
		t.Fatal(err)
	}
	if gaps := captureGaps(request); len(gaps) != 0 {
		t.Fatalf("gaps=%+v", gaps)
	}
}

// A parent retention has forgotten has nobody to tell why its subagent is
// missing: the rejection still acknowledges the candidate instead of
// failing, as it does for the link.
func TestNeverWrittenSubagentOfAForgottenParentIsAcknowledged(t *testing.T) {
	t.Parallel()
	local, stopAt, _ := subagentGraceFixture(t, archive.SessionOriginHook)
	orphan := state.SubagentCandidate{ArchiveSessionID: "orphan", NativeSessionID: "gone-native:subagent:agent-2", ParentArchiveSessionID: "gone", ParentNativeSessionID: "gone-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-2", TranscriptPath: "/never/written.jsonl", ObservedAt: stopAt, AgentType: "Plan"}
	if err := local.SaveSubagentCandidate(orphan); err != nil {
		t.Fatal(err)
	}
	var rejected subagentRejectedError
	if err := rejectSubagentCandidate(local, orphan, subagentNeverWritten); !errors.As(err, &rejected) || rejected.code != subagentNeverWritten {
		t.Fatalf("err=%v, want the rejection", err)
	}
	if _, found, err := local.LoadRequest("gone"); err != nil || found {
		t.Fatalf("a request was written for a forgotten parent: found=%t err=%v", found, err)
	}
	if n := pendingCandidates(t, local); n != 1 {
		t.Fatalf("candidates=%d, want only the fixture's left", n)
	}
}

// The parent's published metadata says why its subagent is missing, and
// status lists the subagent, with its type, for a week of passes and no
// longer.
func TestNeverWrittenSubagentReachesParentMetadataAndStatus(t *testing.T) {
	t.Parallel()
	local, stopAt, _ := subagentGraceFixture(t, archive.SessionOriginHook)
	typeSubagentCandidate(t, local, "Explore")
	remote := storagetest.NewMemoryStore()

	runPassAt(t, local, remote, stopAt.Add(time.Minute))
	if status, err := local.LoadStatus(); err != nil || len(status.ExpiredSubagents) != 0 {
		t.Fatalf("a waiting subagent is listed as expired: %+v err=%v", status.ExpiredSubagents, err)
	}
	expiredAt := stopAt.Add(subagentTranscriptGrace)
	if result := runPassAt(t, local, remote, expiredAt); len(result.Errors) != 0 || result.RejectedSubagents["child"] != subagentNeverWritten {
		t.Fatalf("result=%+v", result)
	}
	metadata := fetchMetadata(t, remote, "claude", "parent")
	found := false
	for _, gap := range metadata.CaptureGaps {
		if gap.Code == subagentNeverWritten && gap.Detail == "Claude Code reported a subagent (type Explore) but never wrote its transcript" {
			found = true
		}
	}
	if !found {
		t.Fatalf("parent capture gaps=%+v", metadata.CaptureGaps)
	}
	want := []state.ExpiredSubagent{{ArchiveSessionID: "child", AgentType: "Explore", ExpiredAt: expiredAt}}
	for _, at := range []time.Time{expiredAt, expiredAt.Add(time.Hour), expiredAt.Add(state.ExpiredSubagentWindow - time.Minute)} {
		if at.After(expiredAt) {
			runPassAt(t, local, remote, at)
		}
		status, err := local.LoadStatus()
		if err != nil || !slices.Equal(status.ExpiredSubagents, want) {
			t.Fatalf("status at %s lists %+v (err %v), want %+v", at, status.ExpiredSubagents, err, want)
		}
	}
	runPassAt(t, local, remote, expiredAt.Add(state.ExpiredSubagentWindow))
	if status, err := local.LoadStatus(); err != nil || len(status.ExpiredSubagents) != 0 {
		t.Fatalf("status a week later lists %+v (err %v)", status.ExpiredSubagents, err)
	}
}
