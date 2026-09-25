package collector

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func lockFiles(t *testing.T, home string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "request-locks", "*.lock"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, match := range matches {
		names = append(names, filepath.Base(match))
	}
	sort.Strings(names)
	return names
}

// Rejected subagent candidates, and candidates of a parent retention had
// forgotten, left their lock files (and the forgotten parent a fresh request
// lock) behind for good. Rejecting or acknowledging a candidate now removes
// its lock, and so does a request refused for an unregistered session.
func TestRejectedCandidatesLeaveNoLockFiles(t *testing.T) {
	home := t.TempDir()
	local, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(home, "x.jsonl")
	// Names another parent session, so every candidate is rejected.
	writeTranscript(t, home, "x.jsonl", `{"type":"assistant","sessionId":"other","agentId":"a","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"x"}}`+"\n")
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: start, RegisteredAt: start}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	candidate := func(id, parentID string) state.SubagentCandidate {
		return state.SubagentCandidate{ArchiveSessionID: id, NativeSessionID: id + "-n", ParentArchiveSessionID: parentID, ParentNativeSessionID: parentID + "-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "a", TranscriptPath: path, ObservedAt: start.Add(3 * time.Minute)}
	}
	for _, c := range []state.SubagentCandidate{candidate("c1", "parent"), candidate("c2", "parent"), candidate("c3", "forgotten")} {
		if err := local.SaveSubagentCandidate(c); err != nil {
			t.Fatal(err)
		}
	}
	if errs := materializeSubagentCandidates(local, Options{}); len(errs) != 3 {
		t.Fatalf("errors = %v, want three rejections", errs)
	}
	// Only the registered parent's request lock remains: it now has a
	// request (the rejection notice) and a registration.
	if got := lockFiles(t, home); len(got) != 1 || got[0] != "parent.lock" {
		t.Fatalf("lock files left = %v", got)
	}
}

// Lock files whose session or candidate is gone (left by an older version, or
// by a crash) are swept by the next pass; a registered session's lock, and a
// lock someone holds, are left alone.
func TestPassSweepsOrphanedLockFiles(t *testing.T) {
	store := newTestStore(t)
	home := store.Home()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", `{"type":"turn_context","model":"gpt-test"}`+"\n")
	if err := store.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ghost.lock", "subagent-ghost.lock", "session-1.lock", "held.lock"} {
		if err := os.WriteFile(filepath.Join(home, "request-locks", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unlock, err := local.NamedLock(home, filepath.Join("request-locks", "held.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	runAt(t, store, storagetest.NewMemoryStore(), time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC))
	if got := lockFiles(t, home); len(got) != 2 || got[0] != "held.lock" || got[1] != "session-1.lock" {
		t.Fatalf("lock files left = %v", got)
	}
}
