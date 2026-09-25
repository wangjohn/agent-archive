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
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
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
	remote := storagetest.NewMemoryStore()
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
	remote := storagetest.NewMemoryStore()
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

// TestCursorSQLiteRememberedFailureWithRequest: a hook request on a chat
// whose read failed at the state it is still in costs no copy of the
// database. A filter failure is reported again and the request stays queued
// (its evidence is the only copy); a size-limit gap completes the request,
// as blocking does. A changed chat is read with the request.
//
// Regression: Cursor source P2 second review, 2026-09 (0f99a2d).
func TestCursorSQLiteRememberedFailureWithRequest(t *testing.T) {
	passes := countSnapshots(t)
	local := newTestStore(t)
	db := newCursorDB(t, true)
	db.put("bubbleId:bad:b1", `{"_v":99,"bubbleId":"b1","type":1,"text":"x"}`)
	db.put("composerData:bad", `{"_v":18,"composerId":"bad","lastUpdatedAt":1,"fullConversationHeadersOnly":[{"bubbleId":"b1"}]}`)
	bad := cursorRegistration("bad-session", "bad")
	if err := local.SaveRegistration(bad); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "m", CursorDatabase: db.path, Now: advancingClock()}
	if result, copies := run(t, local, remote, opts, passes); !errors.Is(result.Errors[bad.ArchiveSessionID], archive.ErrUnsafeSourceFormat) || copies != 1 {
		t.Fatalf("%v, %d copies", result.Errors, copies)
	}
	for pass := range 3 {
		if err := local.SaveRequest(bad.ArchiveSessionID, "stop", at.Add(time.Duration(pass)*time.Minute)); err != nil {
			t.Fatal(err)
		}
		result, copies := run(t, local, remote, opts, passes)
		var again unchangedSinceFailureError
		if !errors.As(result.Errors[bad.ArchiveSessionID], &again) || copies != 0 {
			t.Fatalf("pass %d: %v, %d copies", pass, result.Errors, copies)
		}
		if _, queued, err := local.LoadRequest(bad.ArchiveSessionID); err != nil || !queued {
			t.Fatalf("pass %d: request queued %v, %v", pass, queued, err)
		}
	}
	// The chat changes into one the filter accepts: read with the request,
	// published, and the request completed.
	db.chat("bad", 5, "b1")
	result, copies := run(t, local, remote, opts, passes)
	if !contains(result.Published, bad.ArchiveSessionID) || copies != 1 {
		t.Fatalf("%+v, %d copies", result, copies)
	}
	if _, queued, _ := local.LoadRequest(bad.ArchiveSessionID); queued {
		t.Fatal("the request outlived the publication")
	}

	// A chat over the size limit: blocked once; a request on it later is
	// completed without a read.
	db.chat("big", 1, "a", "b")
	big := cursorRegistration("big-session", "big")
	if err := local.SaveRegistration(big); err != nil {
		t.Fatal(err)
	}
	opts.MaxTranscriptBytes = 50
	run(t, local, remote, opts, passes)
	if reason, blocked, _ := local.LoadBlocked(big.ArchiveSessionID); !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("blocked %v %q", blocked, reason)
	}
	if err := local.SaveRequest(big.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	result, copies = run(t, local, remote, opts, passes)
	if copies != 0 || result.Errors[big.ArchiveSessionID] != nil {
		t.Fatalf("%+v, %d copies", result, copies)
	}
	if _, queued, _ := local.LoadRequest(big.ArchiveSessionID); queued {
		t.Fatal("the request on an unchanged gap stayed queued")
	}

	// Raising the limit reads the chat again, although it has not changed.
	opts.MaxTranscriptBytes = 0
	result, copies = run(t, local, remote, opts, passes)
	if !contains(result.Published, big.ArchiveSessionID) || copies != 1 {
		t.Fatalf("after raising the limit: %+v, %d copies", result, copies)
	}
}

// TestCursorSQLiteChatNewerThanTheSnapshot: a chat Cursor wrote after the
// pass's snapshot was taken is in the database but not in the copy. That is
// a read to retry, not a deleted chat: no missing-source gap, and a hook
// request stays queued.
//
// Regression: Cursor source P2 second review, 2026-09 (0f99a2d).
func TestCursorSQLiteChatNewerThanTheSnapshot(t *testing.T) {
	local := newTestStore(t)
	db := newCursorDB(t, true)
	db.chat("old", 1, "m")
	pass := cursorstore.NewReader(db.path)
	defer func() { _ = pass.Close() }()
	if _, _, err := pass.ReadComposer(context.Background(), "old"); err != nil || pass.Snapshots() != 1 {
		t.Fatalf("%v, %d snapshots", err, pass.Snapshots())
	}
	db.chat("new", 1, "m")
	reg := cursorRegistration("session", "new")
	reader := cursorSQLiteReader{reg: reg, dbPath: db.path, pass: pass}
	_, _, err := reader.Filter(context.Background(), archive.CursorAdapter{}, DefaultMaxTranscriptBytes)
	var nc *cursorstore.NotCheckedError
	if errors.Is(err, os.ErrNotExist) || !errors.As(err, &nc) || nc.Reason != cursorstore.ChangedDuringRead {
		t.Fatalf("err %v", err)
	}

	// Through a session: the error is transient, the request stays queued,
	// and nothing is blocked.
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	opts := Options{MachineID: "m", CursorDatabase: db.path, Now: advancingClock(), cursorPass: pass}
	if _, err := processSession(context.Background(), local, storagetest.NewMemoryStore(), reg, mustRequest(t, local, reg.ArchiveSessionID), opts.now(), opts); err == nil {
		t.Fatal("no error")
	}
	if _, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); blocked {
		t.Fatal("blocked as missing")
	}
	if _, queued, _ := local.LoadRequest(reg.ArchiveSessionID); !queued {
		t.Fatal("request completed")
	}
	if _, found, _ := local.LoadScanSignature(reg.ArchiveSessionID); found {
		t.Fatal("a transient failure was remembered")
	}
}

func mustRequest(t *testing.T, local *state.Store, id string) state.Request {
	t.Helper()
	req, found, err := local.LoadRequest(id)
	if err != nil || !found {
		t.Fatalf("request %v %v", found, err)
	}
	return req
}
