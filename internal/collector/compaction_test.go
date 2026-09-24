package collector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// compactionFixtureLines returns the synthetic Claude Code session in
// internal/archive/testdata: two records before a /compact, then the /compact
// command, its boundary, its summary, the command output, a prompt, a reply,
// and an auto-compaction mid-response.
func compactionFixtureLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "archive", "testdata", "claude-compaction.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 11 {
		t.Fatalf("fixture has %d lines, want 11", len(lines))
	}
	return lines
}

func claudeSession(t *testing.T, local *state.Store, content string) string {
	t.Helper()
	path := writeTranscript(t, t.TempDir(), "claude.jsonl", content)
	reg := registration(t, path)
	reg.Harness.Name = "claude"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	return path
}

// Claude Code compacts by appending a boundary and a summary to the same
// transcript; nothing earlier is rewritten. Filtered output extends the
// published records, so the rewrite guard lets the compaction publish like
// any other new activity, with the new counts.
func TestAppendedCompactionPublishesNormally(t *testing.T) {
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	lines := compactionFixtureLines(t)
	path := claudeSession(t, local, strings.Join(lines[:2], "\n")+"\n")
	t0 := time.Date(2026, 9, 22, 12, 0, 30, 0, time.UTC)
	if result := runAt(t, local, remote, t0); len(result.Published) != 1 {
		t.Fatalf("first publication: %#v", result)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Join(lines[2:], "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest("session-1", "stop", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	result := runAt(t, local, remote, t0.Add(time.Hour))
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("the appended compaction did not publish: %#v", result)
	}
	if reason, blocked, _ := local.LoadBlocked("session-1"); blocked {
		t.Fatalf("an appended compaction was treated as a rewrite: %s", reason)
	}
	metadata := fetchMetadata(t, remote, "claude", "session-1")
	if metadata.Counts.Turns == nil || *metadata.Counts.Turns != 2 || metadata.Counts.Compactions == nil || *metadata.Counts.Compactions != 2 {
		t.Fatalf("counts = turns %v compactions %v", metadata.Counts.Turns, metadata.Counts.Compactions)
	}
}

// The guard the previous test relies on: if a compaction ever replaced the
// earlier records instead of appending, the published snapshot is richer than
// the file, and the session is recorded as a gap rather than overwritten.
func TestCompactionThatRewroteTheTranscriptIsRecordedAsAGap(t *testing.T) {
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	lines := compactionFixtureLines(t)
	path := claudeSession(t, local, strings.Join(lines[:2], "\n")+"\n")
	t0 := time.Date(2026, 9, 22, 12, 0, 30, 0, time.UTC)
	runAt(t, local, remote, t0)

	writeTranscript(t, filepath.Dir(path), filepath.Base(path), strings.Join(lines[3:], "\n")+"\n")
	if err := local.SaveRequest("session-1", "stop", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if result := runAt(t, local, remote, t0.Add(time.Hour)); len(result.Published) != 0 || len(result.Errors) != 0 {
		t.Fatalf("result=%#v", result)
	}
	if reason, blocked, _ := local.LoadBlocked("session-1"); !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("a rewriting compaction was not recorded as a gap: blocked=%t reason=%q", blocked, reason)
	}
}
