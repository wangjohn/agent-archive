package state

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	aalocal "github.com/wangjohn/agent-archive/internal/local"
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

// Forgetting a session scans the subagent candidates under its request lock,
// which hooks wait a second for, so the scan does not wait for the lock of a
// candidate that does not decode. Another session's is left where it is
// and the forget goes on; the session's own fails the forget at once, for
// the next attempt to retry.
//
// Regression: the scan waited a second for that lock before moving the
// candidate aside, all under the request lock.
func TestForgettingASessionDoesNotWaitForAnUndecodableCandidatesLock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate string
		forgotten bool
	}{
		{"another session's candidate", "other", true},
		{"the session's own candidate", "session-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(local.subagentCandidatePath(tc.candidate), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			unlock, err := local.lockSubagentCandidate(tc.candidate)
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			forgotten, err := local.ForgetIdleSession(reg.ArchiveSessionID, reg.NativeSessionID, false, nil)
			took := time.Since(started)
			unlock()
			// A wait for the lock lasts its full second before giving up, so
			// anything shorter did not wait.
			if took >= 950*time.Millisecond {
				t.Fatalf("the forget waited %v for the candidate's lock", took)
			}
			if forgotten != tc.forgotten || (tc.forgotten && err != nil) || (!tc.forgotten && !errors.Is(err, aalocal.ErrBusy)) {
				t.Fatalf("forgotten=%t err=%v, want forgotten=%t", forgotten, err, tc.forgotten)
			}
			if _, registered, _ := local.LoadRegistration(reg.ArchiveSessionID); registered == tc.forgotten {
				t.Fatalf("registered=%t after forgotten=%t", registered, forgotten)
			}
			if _, err := os.Stat(local.subagentCandidatePath(tc.candidate)); err != nil {
				t.Fatalf("the candidate whose lock was held was moved: %v", err)
			}
		})
	}
}

// Forgetting an orphaned session does not wait, under its request lock, for
// the lock of a subagent candidate naming it: a held one means a hook or the
// collector is recording that subagent, and the attempt fails at once for
// the next sweep to retry.
//
// Regression: ForgetOrphan waited up to a second for each candidate's lock
// while holding the request lock.
func TestForgettingAnOrphanDoesNotWaitForASubagentCandidateLock(t *testing.T) {
	at := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	local := newTestStore(t)
	child := SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-1:subagent:agent", ParentArchiveSessionID: "orphan", ParentNativeSessionID: "native-1", ProjectID: "p", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/unused", ObservedAt: at}
	if err := local.SaveSubagentCandidate(child); err != nil {
		t.Fatal(err)
	}
	unlock, err := local.lockSubagentCandidate(child.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	forgotten, err := local.ForgetOrphan("orphan")
	took := time.Since(started)
	unlock()
	// A wait for the lock lasts its full second before giving up.
	if took >= 950*time.Millisecond {
		t.Fatalf("the forget waited %v for the candidate's lock", took)
	}
	if forgotten || !errors.Is(err, aalocal.ErrBusy) {
		t.Fatalf("forgotten=%t err=%v, want ErrBusy", forgotten, err)
	}
	if ids, err := local.subagentCandidatesForSession("orphan"); err != nil || len(ids) != 1 {
		t.Fatalf("candidates=%v err=%v, want the child kept", ids, err)
	}

	// Once the lock is free, the orphan and its candidate go.
	if forgotten, err := local.ForgetOrphan("orphan"); err != nil || !forgotten {
		t.Fatalf("retry: forgotten=%t err=%v", forgotten, err)
	}
	if ids, err := local.subagentCandidatesForSession("orphan"); err != nil || len(ids) != 0 {
		t.Fatalf("candidates=%v err=%v, want none", ids, err)
	}
}
