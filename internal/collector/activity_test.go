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
		// A chat that is gone is left out.
		regs = append(regs, cursorRegistration("s-gone", "chat-gone"))
		transcript := filepath.Join(t.TempDir(), "t.jsonl")
		if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(transcript, at, at); err != nil {
			t.Fatal(err)
		}
		regs = append(regs, archive.SessionRegistration{ArchiveSessionID: "s-file", Harness: archive.Harness{Name: "claude"}, TranscriptPath: transcript})

		reads := 0
		previous := readCursorLastUpdated
		readCursorLastUpdated = func(ctx context.Context, dbPath string, ids []string) (map[string]int64, error) {
			reads++
			return previous(ctx, dbPath, ids)
		}
		got := LastActivities(context.Background(), regs, db.path)
		readCursorLastUpdated = previous

		if reads != 1 {
			t.Fatalf("running=%v: %d reads of the Cursor database for %d chats", running, reads, len(regs)-2)
		}
		if len(got) != len(regs)-1 {
			t.Fatalf("running=%v: %d activities, want %d: %v", running, len(got), len(regs)-1, got)
		}
		for _, reg := range regs {
			want, ok := LastActivity(context.Background(), reg, db.path)
			if got[reg.ArchiveSessionID] != want || ok != !got[reg.ArchiveSessionID].IsZero() {
				t.Fatalf("running=%v: %s: LastActivities %v, LastActivity %v (%v)", running, reg.ArchiveSessionID, got[reg.ArchiveSessionID], want, ok)
			}
		}
	}
}
