package collector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The size limit applied to the raw transcript, before the filter dropped
// toolUseResult, so the longest sessions (whose bulk is exactly that tool
// output) stopped being captured at their last snapshot under 64 MiB, with
// nothing in status. It now applies to what the filter keeps; the raw file
// may be rawSizeFactor times larger. Here the limit is scaled down to 64 KiB.
func TestSizeLimitAppliesToTheFilteredTranscript(t *testing.T) {
	const limit = 64 << 10
	transcript := func(results int) string {
		var b strings.Builder
		b.WriteString(claudePromptLine + "\n")
		for i := range results {
			b.WriteString(toolResultLine(fmt.Sprintf("r%d", i), 40<<10) + "\n")
		}
		return b.String()
	}
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	content := transcript(5)
	if len(content) <= limit || len(content) > limit*rawSizeFactor {
		t.Fatalf("test precondition: raw size %d", len(content))
	}
	path := claudeSession(t, local, content)
	at := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", Now: func() time.Time { return at }, MaxTranscriptBytes: limit}
	if r, _ := measurePass(t, local, remote, opts); len(r.Published) != 1 || len(r.Errors) != 0 {
		t.Fatalf("a transcript over the limit only in dropped tool output was not captured: %#v", r)
	}

	// Past the raw ceiling it is a gap without being read, named in status.
	writeTranscript(t, dirOf(path), baseOf(path), transcript(4*rawSizeFactor))
	at = at.Add(time.Hour)
	if r, _ := measurePass(t, local, remote, opts); len(r.Errors) != 0 {
		t.Fatalf("%#v", r.Errors)
	}
	if reason, blocked, _ := local.LoadBlocked("session-1"); !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("reason=%q blocked=%t", reason, blocked)
	}
	if status, _ := local.LoadStatus(); !strings.Contains(status.LastError, "size limit") {
		t.Fatalf("status.LastError = %q", status.LastError)
	}
}

// What the filter keeps is itself limited: a transcript that is all
// retained text past the limit is the same gap.
func TestFilteredTranscriptOverTheLimitIsAGap(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	var b strings.Builder
	b.WriteString(claudePromptLine + "\n")
	for i := range 8 {
		fmt.Fprintf(&b, `{"type":"user","uuid":"u%d","sessionId":"native-claude","timestamp":"2026-09-22T12:00:0%dZ","message":{"role":"user","content":%q}}`+"\n", i, i, strings.Repeat("kept text ", 1000))
	}
	claudeSession(t, local, b.String())
	opts := Options{MachineID: "m", Now: func() time.Time { return time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC) }, MaxTranscriptBytes: 32 << 10}
	if r, _ := measurePass(t, local, remote, opts); len(r.Errors) != 0 || len(r.Published) != 0 {
		t.Fatalf("%#v", r)
	}
	if reason, blocked, _ := local.LoadBlocked("session-1"); !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("reason=%q blocked=%t", reason, blocked)
	}
}
