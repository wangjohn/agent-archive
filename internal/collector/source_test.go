package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// writeCursorChat writes a closed Cursor database at path holding chat id
// with the given message IDs.
func writeCursorChat(t *testing.T, path, id string, lastUpdatedAt int64, bubbles ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	headers := []map[string]any{}
	for _, b := range bubbles {
		headers = append(headers, map[string]any{"bubbleId": b, "type": 1})
	}
	value, _ := json.Marshal(map[string]any{"_v": 18, "composerId": id, "lastUpdatedAt": lastUpdatedAt, "fullConversationHeadersOnly": headers})
	for _, stmt := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "composerData:"+id, string(value)); err != nil {
		t.Fatal(err)
	}
	for _, b := range bubbles {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "bubbleId:"+id+":"+b, `{"type":1,"text":"hi"}`); err != nil {
			t.Fatal(err)
		}
	}
}

func cursorRegistration(id, composerID string) archive.SessionRegistration {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return archive.SessionRegistration{
		ArchiveSessionID: id, NativeSessionID: composerID, ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "cursor"}, SessionStartedAt: at, RegisteredAt: at,
		SourceKind: archive.SourceKindCursorSQLite, SourceKey: composerID,
	}
}

// settleCursorSession records the scan signature a completed scan of the
// chat as it is now would leave. Filter is not wired yet, so no pass can.
func settleCursorSession(t *testing.T, local *LocalStore, reg archive.SessionRegistration, opts Options) {
	t.Helper()
	sig, err := cursorstore.ReadSignature(context.Background(), opts.CursorDatabase, reg.SourceKey)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		t.Fatal(err)
	}
	bundle := archive.SourceBundle{Capture: archive.SourceCapture{FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), SourceFormat: "cursor-composer"}}
	if err := recordScanSignature(local, reg, sourceState{kind: archive.SourceKindCursorSQLite, cursor: sig}, bundle, opts); err != nil {
		t.Fatal(err)
	}
	// A completed scan also clears the journal a failed one left.
	if err := local.SetScanPending(reg.ArchiveSessionID, false); err != nil {
		t.Fatal(err)
	}
}

// TestCursorSQLiteSourceChangeDetection: a cursor-sqlite session is skipped
// on an unchanged chat Signature, whatever happens to the database file, and
// read once the chat changes. Until the composer adapter is wired, that read
// is a recorded not-wired error that neither fails the pass nor holds up
// another session.
func TestCursorSQLiteSourceChangeDetection(t *testing.T) {
	local := newTestStore(t)
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writeCursorChat(t, dbPath, "chat-1", 1000, "b1", "b2")
	opts := Options{MachineID: "m", CursorDatabase: dbPath, Now: func() time.Time { return time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC) }}

	reg := cursorRegistration("cursor-session", "chat-1")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	file := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	if err := local.SaveRegistration(file); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()

	// Never scanned: the chat is read, and the not-wired error is recorded
	// for it alone.
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Errors[reg.ArchiveSessionID], errCursorSourceNotWired) {
		t.Fatalf("errors %v", result.Errors)
	}
	if len(result.Published) != 1 || result.Published[0] != file.ArchiveSessionID || result.Errors[file.ArchiveSessionID] != nil {
		t.Fatalf("the file session was held up: %+v", result)
	}
	if _, found, _ := local.loadScanSignature(reg.ArchiveSessionID); found {
		t.Fatal("a failed read left a scan signature")
	}

	// Settled: another chat's write, and a rewrite of the whole file, leave
	// the chat's Signature alone, so the session is skipped unread.
	settleCursorSession(t, local, reg, opts)
	writeCursorChat(t, dbPath, "chat-2", 5, "x")
	writeCursorChat(t, dbPath, "chat-1", 1000, "b1", "b2")
	if unchanged, err := unchangedSinceLastScan(local, reg, opts); err != nil || !unchanged {
		t.Fatalf("unchanged=%v err=%v", unchanged, err)
	}
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || !contains(result.Skipped, reg.ArchiveSessionID) {
		t.Fatalf("an unchanged chat was read: %+v", result)
	}

	// A new message changes the Signature: the chat is read again.
	writeCursorChat(t, dbPath, "chat-1", 1000, "b1", "b2", "b3")
	if unchanged, err := unchangedSinceLastScan(local, reg, opts); err != nil || unchanged {
		t.Fatalf("unchanged=%v err=%v", unchanged, err)
	}
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(result.Errors[reg.ArchiveSessionID], errCursorSourceNotWired) {
		t.Fatalf("errors %v", result.Errors)
	}

	// So does lastUpdatedAt alone.
	settleCursorSession(t, local, reg, opts)
	writeCursorChat(t, dbPath, "chat-1", 2000, "b1", "b2", "b3")
	if unchanged, err := unchangedSinceLastScan(local, reg, opts); err != nil || unchanged {
		t.Fatalf("unchanged=%v err=%v", unchanged, err)
	}

	// A chat that can't be read is never unchanged.
	settleCursorSession(t, local, reg, opts)
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if unchanged, err := unchangedSinceLastScan(local, reg, opts); err != nil || unchanged {
		t.Fatalf("missing database: unchanged=%v err=%v", unchanged, err)
	}
	if entries, _ := os.ReadDir(local.cursorSnapshotDir()); len(entries) != 0 {
		t.Fatalf("snapshots left behind: %v", entries)
	}
}

// TestCursorSQLiteSourceMissingChat: a chat Cursor deleted is a recorded
// missing-source gap, as a deleted transcript is, not an error every pass.
func TestCursorSQLiteSourceMissingChat(t *testing.T) {
	local := newTestStore(t)
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writeCursorChat(t, dbPath, "other", 1, "x")
	reg := cursorRegistration("cursor-session", "gone")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "m", CursorDatabase: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("errors %v", result.Errors)
	}
	reason, found, err := local.LoadBlocked(reg.ArchiveSessionID)
	if err != nil || !found || reason != BlockedReasonTranscriptMissing {
		t.Fatalf("blocked %q found=%v err=%v", reason, found, err)
	}
}

func TestFileSourceStateMatchesOnlyFileSignatures(t *testing.T) {
	file := sourceState{file: transcriptFileInfo{Size: 3, Mtime: 4}}
	if !file.matches(scanSignature{TranscriptSize: 3, TranscriptMtime: 4}) {
		t.Fatal("a file signature did not match its own state")
	}
	if file.matches(scanSignature{TranscriptSize: 3, TranscriptMtime: 4, SourceKind: archive.SourceKindCursorSQLite}) {
		t.Fatal("a cursor signature matched a file")
	}
	chat := sourceState{kind: archive.SourceKindCursorSQLite}
	if chat.matches(scanSignature{}) {
		t.Fatal("a file signature matched a chat")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
