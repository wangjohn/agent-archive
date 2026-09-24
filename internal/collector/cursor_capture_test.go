package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// rewriteGaps counts the cursor_chat_rewritten gaps in gaps and returns the
// last one's detail.
func rewriteGaps(gaps []archive.CaptureGap) (int, string) {
	n, detail := 0, ""
	for _, gap := range gaps {
		if gap.Code == CaptureGapCursorChatRewritten {
			n++
			detail = gap.Detail
		}
	}
	return n, detail
}

// TestCursorSQLiteRewrittenChatRepublishes: Cursor rewriting a finished
// message is not a permanent block, as it is for a transcript file (see
// TestRunPreservesLastGoodSnapshotAcrossTranscriptRewrite): the new snapshot is
// published in place of the old, with one cursor_chat_rewritten gap that
// counts the rewrites (so the source and metadata don't grow with them), and
// later appends publish as usual.
func TestCursorSQLiteRewrittenChatRepublishes(t *testing.T) {
	passes := countSnapshots(t)
	local := newTestStore(t)
	db := newCursorDB(t, true)
	db.chatSaying("chat", 1000, "first draft", "b1", "b2")
	reg := cursorRegistration("session", "chat")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	opts := Options{MachineID: "m", CursorDatabase: db.path, Now: advancingClock()}
	if result, _ := run(t, local, remote, opts, passes); len(result.Published) != 1 {
		t.Fatalf("first capture: %+v", result)
	}

	for rewrite := 1; rewrite <= 4; rewrite++ {
		text := strings.Repeat("edited ", rewrite)
		db.chatSaying("chat", int64(1000+rewrite), text, "b1", "b2")
		result, _ := run(t, local, remote, opts, passes)
		if len(result.Errors) != 0 || !contains(result.Published, reg.ArchiveSessionID) {
			t.Fatalf("rewrite %d: %+v", rewrite, result)
		}
		if _, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); blocked {
			t.Fatalf("rewrite %d blocked the chat", rewrite)
		}
		counted := fmt.Sprintf(" %d time(s)", rewrite)
		bundle, _, _, _ := local.LoadLastPublished(reg.ArchiveSessionID)
		if n, detail := rewriteGaps(bundle.Capture.Gaps); n != 1 || !strings.Contains(detail, counted) {
			t.Fatalf("rewrite %d: %d rewrite gaps, %q", rewrite, n, detail)
		}
		rewriteEvidence := 0
		for _, e := range bundle.SupplementalEvidence {
			if e.Provenance == cursorRewriteProvenance {
				rewriteEvidence++
			}
		}
		if rewriteEvidence != 1 {
			t.Fatalf("rewrite %d: %d rewrite evidence items", rewrite, rewriteEvidence)
		}
		metadata := fetchMetadata(t, remote, "cursor", reg.ArchiveSessionID)
		if n, detail := rewriteGaps(metadata.CaptureGaps); n != 1 || !strings.Contains(detail, counted) {
			t.Fatalf("rewrite %d: metadata has %d rewrite gaps, %q", rewrite, n, detail)
		}
		if records := fetchBundle(t, remote, metadata); !strings.Contains(recordsText(records), text) || strings.Contains(recordsText(records), "first draft") {
			t.Fatal("the published snapshot is not the rewritten chat")
		}
	}

	// An append after a rewrite is ordinary: published, no new gap. An
	// unchanged chat then costs nothing and keeps its gaps.
	db.chatSaying("chat", 2000, strings.Repeat("edited ", 4), "b1", "b2", "b3")
	if result, _ := run(t, local, remote, opts, passes); !contains(result.Published, reg.ArchiveSessionID) {
		t.Fatalf("append: %+v", result)
	}
	result, copies := run(t, local, remote, opts, passes)
	if !contains(result.Skipped, reg.ArchiveSessionID) || copies != 0 {
		t.Fatalf("unchanged: %+v, %d copies", result, copies)
	}
	bundle, _, _, _ := local.LoadLastPublished(reg.ArchiveSessionID)
	if n, detail := rewriteGaps(bundle.Capture.Gaps); n != 1 || !strings.Contains(detail, " 4 time(s)") {
		t.Fatalf("after an append: %d rewrite gaps, %q", n, detail)
	}
}

// recordsText is a bundle's native records as one string, for a content
// check.
func recordsText(bundle archive.SourceBundle) string {
	var b strings.Builder
	for _, r := range bundle.NativeRecords {
		for _, v := range r {
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(fmtAny(v), "\n", " ")))
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func fmtAny(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case map[string]any:
		var parts []string
		for _, x := range v {
			parts = append(parts, fmtAny(x))
		}
		return strings.Join(parts, " ")
	case []any:
		var parts []string
		for _, x := range v {
			parts = append(parts, fmtAny(x))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// TestCursorSQLiteMetadataRegeneration: a parser upgrade on an unchanged
// Cursor chat republishes its metadata from the retained source without
// reading the chat into a new snapshot; a chat Cursor rewrote meanwhile is
// left to normal capture, which republishes it.
func TestCursorSQLiteMetadataRegeneration(t *testing.T) {
	local := newTestStore(t)
	db := newCursorDB(t, false)
	db.chat("chat", 1, "b1")
	reg := cursorRegistration("session", "chat")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	opts := Options{MachineID: "m", CursorDatabase: db.path, Now: advancingClock(), ParserVersion: "one"}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	before := fetchMetadata(t, remote, "cursor", reg.ArchiveSessionID)

	opts.ParserVersion = "two"
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	after := fetchMetadata(t, remote, "cursor", reg.ArchiveSessionID)
	if after.Parser.Version != "two" || after.SourceBundle != before.SourceBundle {
		t.Fatalf("metadata-only regeneration: %+v -> %+v", before.SourceBundle, after.SourceBundle)
	}

	// Rewritten, then upgraded: capture publishes the new source, with the
	// new parser, rather than regenerating over the old one.
	db.chatSaying("chat", 2, "rewritten", "b1")
	opts.ParserVersion = "three"
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	final := fetchMetadata(t, remote, "cursor", reg.ArchiveSessionID)
	if final.Parser.Version != "three" || final.SourceBundle == after.SourceBundle || !strings.Contains(recordsText(fetchBundle(t, remote, final)), "rewritten") {
		t.Fatalf("rewritten chat not captured: %+v", final.SourceBundle)
	}
}

// TestCursorSQLiteReadLocalBundle: `handoff --source local` reads a Cursor
// database chat as a pass would, and its last activity is the chat's
// lastUpdatedAt; a chat Cursor deleted is ErrNoTranscript.
func TestCursorSQLiteReadLocalBundle(t *testing.T) {
	home := t.TempDir()
	db := newCursorDB(t, true)
	db.chatSaying("chat", 1767225700000, "hand this off", "b1")
	reg := cursorRegistration("session", "chat")
	at := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	bundle, err := ReadLocalBundle(context.Background(), home, reg, at, db.path)
	if err != nil || bundle.Capture.SourceFormat != "cursor-composer" || !strings.Contains(recordsText(bundle), "hand this off") {
		t.Fatalf("%v: %+v", err, bundle.Capture)
	}
	if last, ok := LastActivity(context.Background(), reg, db.path); !ok || !last.Equal(time.UnixMilli(1767225700000)) {
		t.Fatalf("last activity %v %v", last, ok)
	}
	root, err := cursorstore.SnapshotRoot()
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("snapshot left: %v", entries)
	}
	reg.SourceKey, reg.NativeSessionID = "gone", "gone"
	if _, err := ReadLocalBundle(context.Background(), home, reg, at, db.path); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("deleted chat: %v", err)
	}
}
