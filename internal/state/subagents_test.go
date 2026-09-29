package state

import (
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// A repeated SubagentStop keeps the first type any delivery named: a later
// type neither replaces it nor conflicts with the candidate's ownership, and
// a later delivery without one does not erase it.
func TestDuplicateSubagentStopsKeepTheFirstType(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	candidate := SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-parent:subagent:agent", ParentArchiveSessionID: "parent", ParentNativeSessionID: "native-parent", ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at}
	load := func() SubagentCandidate {
		t.Helper()
		candidates, err := store.LoadSubagentCandidates()
		if err != nil || len(candidates) != 1 {
			t.Fatalf("candidates=%+v err=%v", candidates, err)
		}
		return candidates[0]
	}

	// A first stop without a type takes the first one that names one.
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if got := load().AgentType; got != "" {
		t.Fatalf("type=%q, want none", got)
	}
	explore := candidate
	explore.AgentType, explore.ObservedAt = "Explore", at.Add(time.Second)
	if err := store.SaveSubagentCandidate(explore); err != nil {
		t.Fatal(err)
	}
	if got := load().AgentType; got != "Explore" {
		t.Fatalf("type=%q, want Explore", got)
	}

	plan := candidate
	plan.AgentType, plan.ObservedAt = "Plan", at.Add(2*time.Second)
	if err := store.SaveSubagentCandidate(plan); err != nil {
		t.Fatalf("a different type is not an ownership change: %v", err)
	}
	untyped := candidate
	untyped.ObservedAt = at.Add(3 * time.Second)
	if err := store.SaveSubagentCandidate(untyped); err != nil {
		t.Fatal(err)
	}
	got := load()
	if got.AgentType != "Explore" || !got.ObservedAt.Equal(at.Add(3*time.Second)) {
		t.Fatalf("candidate=%+v, want the first type and the latest stop", got)
	}

	// Ownership still cannot change, type or no type.
	moved := explore
	moved.TranscriptPath = "/elsewhere"
	if err := store.SaveSubagentCandidate(moved); !errors.Is(err, ErrSubagentCandidateConflict) {
		t.Fatalf("err=%v, want a conflict", err)
	}
}
