package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// LastActivities reads the Cursor database once for all its chats, and
// agrees with LastActivity for every registration. Not parallel: it swaps
// readCursorLastUpdated.
func TestLastActivitiesReadsTheCursorDatabaseOnce(t *testing.T) {
	for _, running := range []bool{false, true} {
		db := newCursorDB(t, running)
		var regs []archive.SessionRegistration
		for i := range 40 {
			chat := fmt.Sprintf("chat-%d", i)
			db.chat(chat, int64(1767225600000+i*1000), "b1")
			regs = append(regs, cursorRegistration(fmt.Sprintf("s-%d", i), chat))
		}
		// A chat that is gone, unreadable, or has no lastUpdatedAt is left
		// out, as LastActivity leaves it out.
		regs = append(regs, cursorRegistration("s-gone", "chat-gone"))
		db.put("composerData:chat-bad", "{not json")
		regs = append(regs, cursorRegistration("s-bad", "chat-bad"))
		db.put("composerData:chat-null", `{"lastUpdatedAt":null}`)
		regs = append(regs, cursorRegistration("s-null", "chat-null"))
		db.chat("chat-zero", 0, "b1")
		regs = append(regs, cursorRegistration("s-zero", "chat-zero"))
		db.put("composerData:chat-string", `{"lastUpdatedAt":"soon"}`)
		regs = append(regs, cursorRegistration("s-string", "chat-string"))
		regs = append(regs, cursorRegistration("s-nokey", ""))
		// Two sessions reading one chat both get its activity.
		regs = append(regs, cursorRegistration("s-again", "chat-3"))
		const missing = 7 // gone, bad, null, zero, string, no key, no file
		transcript := filepath.Join(t.TempDir(), "t.jsonl")
		if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(transcript, at, at); err != nil {
			t.Fatal(err)
		}
		regs = append(regs, archive.SessionRegistration{ArchiveSessionID: "s-file", Harness: archive.Harness{Name: "claude"}, TranscriptPath: transcript})
		regs = append(regs, archive.SessionRegistration{ArchiveSessionID: "s-nofile", Harness: archive.Harness{Name: "codex"}, TranscriptPath: filepath.Join(t.TempDir(), "gone.jsonl")})

		reads := 0
		previous := readCursorLastUpdated
		readCursorLastUpdated = func(ctx context.Context, dbPath string, ids []string) (map[string]int64, error) {
			reads++
			return previous(ctx, dbPath, ids)
		}
		got := LastActivities(context.Background(), regs, db.path)
		readCursorLastUpdated = previous

		if reads != 1 {
			t.Fatalf("running=%v: %d reads of the Cursor database", running, reads)
		}
		if len(got) != len(regs)-missing {
			t.Fatalf("running=%v: %d activities, want %d: %v", running, len(got), len(regs)-missing, got)
		}
		if !got["s-again"].Equal(time.UnixMilli(1767225603000)) || !got["s-file"].Equal(at) {
			t.Fatalf("running=%v: shared chat %v, file %v", running, got["s-again"], got["s-file"])
		}
		for _, reg := range regs {
			want, ok := LastActivity(context.Background(), reg, db.path)
			if got[reg.ArchiveSessionID] != want || ok != !got[reg.ArchiveSessionID].IsZero() {
				t.Fatalf("running=%v: %s: LastActivities %v, LastActivity %v (%v)", running, reg.ArchiveSessionID, got[reg.ArchiveSessionID], want, ok)
			}
		}
	}
}

// More chats than SQLite binds variables in one statement are all read.
// Not parallel: newCursorDB swaps the snapshot directory.
func TestLastActivitiesManyChats(t *testing.T) {
	db := newCursorDB(t, false)
	conn := db.open()
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var regs []archive.SessionRegistration
	for i := range 1200 {
		chat := fmt.Sprintf("chat-%d", i)
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "composerData:"+chat, fmt.Sprintf(`{"lastUpdatedAt":%d}`, 1767225600000+i)); err != nil {
			t.Fatal(err)
		}
		regs = append(regs, cursorRegistration(fmt.Sprintf("s-%d", i), chat))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	got := LastActivities(context.Background(), regs, db.path)
	if len(got) != len(regs) || !got["s-1199"].Equal(time.UnixMilli(1767225601199)) {
		t.Fatalf("%d activities for %d chats; s-1199 = %v", len(got), len(regs), got["s-1199"])
	}
}
