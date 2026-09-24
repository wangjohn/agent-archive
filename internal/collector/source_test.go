package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// cursorDB stands in for Cursor's state.vscdb in a test. Running, it holds a
// connection open in WAL mode with checkpoints off, as Cursor does, so the
// -wal and -shm files exist and every chat read needs a snapshot; closed,
// each write opens and closes the database, leaving no side file.
type cursorDB struct {
	t    *testing.T
	path string
	held *sql.DB
}

func newCursorDB(t *testing.T, running bool) *cursorDB {
	t.Helper()
	// Snapshots go to the system temporary directory; give the test its own.
	t.Setenv("TMPDIR", t.TempDir())
	d := &cursorDB{t: t, path: filepath.Join(t.TempDir(), "state.vscdb")}
	if running {
		d.held = d.open()
		if _, err := d.held.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.held.Close() })
	}
	return d
}

func (d *cursorDB) open() *sql.DB {
	d.t.Helper()
	db, err := sql.Open("sqlite", d.path)
	if err != nil {
		d.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`} {
		if _, err := db.Exec(stmt); err != nil {
			d.t.Fatal(err)
		}
	}
	return db
}

func (d *cursorDB) put(key, value string) {
	d.t.Helper()
	db := d.held
	if db == nil {
		db = d.open()
		defer db.Close()
	}
	if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, key, value); err != nil {
		d.t.Fatal(err)
	}
}

// chat writes chat id with the given message IDs, each with a row the
// composer filter accepts: the person's messages, saying "hi".
func (d *cursorDB) chat(id string, lastUpdatedAt int64, bubbles ...string) {
	d.t.Helper()
	d.chatSaying(id, lastUpdatedAt, "hi", bubbles...)
}

// chatSaying is chat with every message's text.
func (d *cursorDB) chatSaying(id string, lastUpdatedAt int64, text string, bubbles ...string) {
	d.t.Helper()
	headers := []map[string]any{}
	for _, b := range bubbles {
		headers = append(headers, map[string]any{"bubbleId": b, "type": 1})
		row, _ := json.Marshal(map[string]any{"_v": 3, "bubbleId": b, "type": 1, "text": text, "createdAt": 1767225600000})
		d.put("bubbleId:"+id+":"+b, string(row))
	}
	value, _ := json.Marshal(map[string]any{"_v": 18, "composerId": id, "createdAt": 1767225600000, "lastUpdatedAt": lastUpdatedAt, "status": "completed", "fullConversationHeadersOnly": headers})
	d.put("composerData:"+id, string(value))
}

func cursorRegistration(id, composerID string) archive.SessionRegistration {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return archive.SessionRegistration{
		ArchiveSessionID: id, NativeSessionID: composerID, ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "cursor"}, SessionStartedAt: at, RegisteredAt: at,
		SourceKind: archive.SourceKindCursorSQLite, SourceKey: composerID,
	}
}

// countSnapshots counts the database copies each pass takes.
func countSnapshots(t *testing.T) *[]int {
	t.Helper()
	var passes []int
	afterCursorPass = func(n int) { passes = append(passes, n) }
	t.Cleanup(func() { afterCursorPass = nil })
	return &passes
}

// run is one pass that must not fail as a whole; it returns the pass's
// result and how many copies of the database it took.
func run(t *testing.T, local *LocalStore, remote storage.ObjectStore, opts Options, passes *[]int) (Result, int) {
	t.Helper()
	before := len(*passes)
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(*passes) != before+1 {
		t.Fatalf("the pass ran %d Cursor readers", len(*passes)-before)
	}
	return result, (*passes)[len(*passes)-1]
}

// settleCursorSession records the scan signature a completed scan of the
// chat as it is now would leave, without a pass, for a state no pass could
// capture (a composerData value the filter refuses).
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

// advancingClock is a clock that moves an hour on every reading, so no
// publication is held back by the upload interval.
func advancingClock() func() time.Time {
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	return func() time.Time {
		now = now.Add(time.Hour)
		return now
	}
}

func unchanged(t *testing.T, local *LocalStore, reg archive.SessionRegistration, opts Options) bool {
	t.Helper()
	u, err := unchangedSinceLastScan(context.Background(), local, reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestCursorSQLiteSourceChangeDetection: with Cursor running, a
// cursor-sqlite session is read (one copy of the database) only when its
// chat's Signature changed, and published as format cursor-composer. A read
// the filter refuses fails neither the pass nor another session, and is
// remembered at the chat's state: later passes report it again without
// reading, until the chat changes.
func TestCursorSQLiteSourceChangeDetection(t *testing.T) {
	passes := countSnapshots(t)
	local := newTestStore(t)
	db := newCursorDB(t, true)
	db.chat("chat-1", 1000, "b1", "b2")
	opts := Options{MachineID: "m", CursorDatabase: db.path, Now: advancingClock()}

	reg := cursorRegistration("cursor-session", "chat-1")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	file := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	if err := local.SaveRegistration(file); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()

	// Never scanned: the chat is read and published, beside the file.
	result, copies := run(t, local, remote, opts, passes)
	if len(result.Errors) != 0 || len(result.Published) != 2 || copies != 1 {
		t.Fatalf("%+v, %d copies", result, copies)
	}
	if bundle, _, found, _ := local.LoadLastPublished(reg.ArchiveSessionID); !found || bundle.Capture.SourceFormat != "cursor-composer" || len(bundle.NativeRecords) != 3 {
		t.Fatalf("published %v: %+v", found, bundle.Capture)
	}

	// Unchanged: skipped without a read.
	for i := 0; i < 2; i++ {
		result, copies = run(t, local, remote, opts, passes)
		if len(result.Errors) != 0 || !contains(result.Skipped, reg.ArchiveSessionID) || copies != 0 {
			t.Fatalf("pass %d: %+v, %d copies", i, result, copies)
		}
	}
	// A new message: read and published again.
	db.chat("chat-1", 1000, "b1", "b2", "b3")
	result, copies = run(t, local, remote, opts, passes)
	if len(result.Errors) != 0 || !contains(result.Published, reg.ArchiveSessionID) || copies != 1 {
		t.Fatalf("%+v, %d copies", result, copies)
	}

	// A message the filter refuses (an unknown format version): the failure
	// is reported for the chat alone, then again without a read.
	db.put("bubbleId:chat-1:b4", `{"_v":99,"bubbleId":"b4","type":1,"text":"new"}`)
	db.put("composerData:chat-1", `{"_v":18,"composerId":"chat-1","lastUpdatedAt":1500,"fullConversationHeadersOnly":[{"bubbleId":"b1"},{"bubbleId":"b2"},{"bubbleId":"b3"},{"bubbleId":"b4"}]}`)
	result, copies = run(t, local, remote, opts, passes)
	if !errors.Is(result.Errors[reg.ArchiveSessionID], archive.ErrUnsafeSourceFormat) || copies != 1 {
		t.Fatalf("errors %v, %d copies", result.Errors, copies)
	}
	for i := 0; i < 2; i++ {
		result, copies = run(t, local, remote, opts, passes)
		var again errUnchangedSinceFailure
		if !errors.As(result.Errors[reg.ArchiveSessionID], &again) || !strings.Contains(again.Error(), "unsafe") || copies != 0 {
			t.Fatalf("pass %d: errors %v, %d copies", i, result.Errors, copies)
		}
	}

	// Settled: another chat's writes leave the chat's Signature alone, so
	// the session is skipped unread.
	settleCursorSession(t, local, reg, opts)
	db.chat("chat-2", 5, "x")
	result, copies = run(t, local, remote, opts, passes)
	if len(result.Errors) != 0 || !contains(result.Skipped, reg.ArchiveSessionID) || copies != 0 {
		t.Fatalf("an unchanged chat was read: %+v, %d copies", result, copies)
	}

	// lastUpdatedAt alone, a late row, and an edit to the last message each
	// change it.
	// In order: each step builds on the last.
	for _, step := range []struct {
		name   string
		change func()
	}{
		{"lastUpdatedAt", func() { db.chat("chat-1", 2000, "b1", "b2", "b3") }},
		{"a header whose row is not there yet", func() {
			db.put("composerData:chat-1", `{"composerId":"chat-1","lastUpdatedAt":2000,"fullConversationHeadersOnly":[{"bubbleId":"b1"},{"bubbleId":"late"}]}`)
		}},
		{"its row arrives", func() { db.put("bubbleId:chat-1:late", `{"text":"late"}`) }},
		{"the last message is edited", func() { db.put("bubbleId:chat-1:late", `{"text":"late, edited"}`) }},
	} {
		settleCursorSession(t, local, reg, opts)
		if !unchanged(t, local, reg, opts) {
			t.Fatalf("before %s: not settled", step.name)
		}
		step.change()
		if unchanged(t, local, reg, opts) {
			t.Fatalf("%s: skipped", step.name)
		}
	}

	// A chat that can't be read is never unchanged.
	settleCursorSession(t, local, reg, opts)
	db.held.Close()
	if err := os.Remove(db.path); err != nil {
		t.Fatal(err)
	}
	if unchanged(t, local, reg, opts) {
		t.Fatal("missing database: skipped")
	}
}

// TestCursorSQLiteOneSnapshotPerPass: however many chats changed, a pass
// copies the database once, and removes the copy when it ends.
func TestCursorSQLiteOneSnapshotPerPass(t *testing.T) {
	passes := countSnapshots(t)
	local := newTestStore(t)
	db := newCursorDB(t, true)
	opts := Options{MachineID: "m", CursorDatabase: db.path}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("chat-%d", i)
		db.chat(id, 1, "m")
		if err := local.SaveRegistration(cursorRegistration("session-"+id, id)); err != nil {
			t.Fatal(err)
		}
	}
	result, copies := run(t, local, storage.NewMemoryStore(), opts, passes)
	if len(result.Errors) != 0 || len(result.Published) != 4 || copies != 1 {
		t.Fatalf("%+v, %d copies", result, copies)
	}
	root, err := cursorstore.SnapshotRoot()
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("snapshots left after the pass: %v", entries)
	}
}

// TestCursorSQLiteFailuresCostNoCopies: with Cursor running, a chat Cursor
// deleted, a composerData value that doesn't decode, and a chat over the
// size limit copy the database at most once, and never on later passes.
func TestCursorSQLiteFailuresCostNoCopies(t *testing.T) {
	for name, tc := range map[string]struct {
		setup     func(db *cursorDB)
		maxBytes  int64
		firstCopy int
		check     func(t *testing.T, local *LocalStore, id string, err error)
	}{
		"missing chat": {func(db *cursorDB) { db.chat("other", 1, "x") }, 0, 0, func(t *testing.T, local *LocalStore, id string, err error) {
			if reason, found, _ := local.LoadBlocked(id); err != nil || !found || reason != BlockedReasonTranscriptMissing {
				t.Fatalf("err %v, blocked %q", err, reason)
			}
		}},
		"unsafe format": {func(db *cursorDB) { db.put("composerData:chat", "not json") }, 0, 0, func(t *testing.T, local *LocalStore, id string, err error) {
			var nc *cursorstore.NotCheckedError
			if !errors.As(err, &nc) || nc.Reason != cursorstore.UnknownFormat {
				t.Fatalf("err %v", err)
			}
		}},
		"too large": {func(db *cursorDB) { db.chat("chat", 1, "a", "b") }, 10, 1, func(t *testing.T, local *LocalStore, id string, err error) {
			if reason, found, _ := local.LoadBlocked(id); err != nil || !found || reason != BlockedReasonTranscriptTooLarge {
				t.Fatalf("err %v, blocked %q", err, reason)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			passes := countSnapshots(t)
			local := newTestStore(t)
			db := newCursorDB(t, true)
			tc.setup(db)
			reg := cursorRegistration("session", "chat")
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			opts := Options{MachineID: "m", CursorDatabase: db.path, MaxTranscriptBytes: tc.maxBytes}
			for pass := 0; pass < 3; pass++ {
				result, copies := run(t, local, storage.NewMemoryStore(), opts, passes)
				want := 0
				if pass == 0 {
					want = tc.firstCopy
				}
				if copies != want {
					t.Fatalf("pass %d: %d copies, want %d", pass, copies, want)
				}
				tc.check(t, local, reg.ArchiveSessionID, result.Errors[reg.ArchiveSessionID])
			}
		})
	}
}

// TestCursorSQLitePassSweepsStaleSnapshots: a pass with a Cursor session
// removes snapshots a killed pass left, even when it reads no chat.
func TestCursorSQLitePassSweepsStaleSnapshots(t *testing.T) {
	local := newTestStore(t)
	db := newCursorDB(t, false)
	db.chat("chat", 1, "m")
	reg := cursorRegistration("session", "chat")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	opts := Options{MachineID: "m", CursorDatabase: db.path}
	settleCursorSession(t, local, reg, opts)
	root, err := cursorstore.SnapshotRoot()
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "cursor-snapshot-stale")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, storage.NewMemoryStore(), opts)
	if err != nil || !contains(result.Skipped, reg.ArchiveSessionID) {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale snapshot kept: %v", err)
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
