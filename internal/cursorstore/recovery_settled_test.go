package cursorstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// kill ends the writer without closing its database, as a crash (or a quit
// that leaves the side files) does, and waits until it has exited.
func (w *writer) kill() {
	w.t.Helper()
	if err := w.cmd.Process.Kill(); err != nil {
		w.t.Fatal(err)
	}
	// The error only reports the kill; the cleanup's Wait reports nothing new.
	_, _ = w.cmd.Process.Wait()
}

// settledWriter leaves chatRows in a WAL database whose -wal is empty
// (checkpointed and truncated) beside a -shm, with the writer still running
// unless killed.
func settledWriter(t *testing.T, path string, killed bool) *writer {
	t.Helper()
	w := startWriter(t, path)
	w.put(chatRows())
	w.do(writerCommand{Op: writerCheckpoint})
	if killed {
		w.kill()
	}
	if !emptyFile(path + "-wal") {
		t.Fatal("the -wal is not empty")
	}
	if _, err := os.Lstat(path + "-shm"); err != nil {
		t.Fatal("no -shm", err)
	}
	return w
}

// TestRecoveryReadsSettledEmptyWAL: a WAL database whose -wal is empty holds
// every committed write in the file itself, so recovery reads it immutably
// whether Cursor left -shm beside it or not, and whether Cursor quit or is
// open and idle. Nothing beside it changes and nothing is copied.
func TestRecoveryReadsSettledEmptyWAL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		killed  bool
		dropShm bool
	}{{"quit, -wal and -shm left", true, false}, {"quit, only -wal left", true, true}, {"running, idle", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			root := useTempSnapshots(t)
			path := StateDatabase(t.TempDir())
			settledWriter(t, path, tc.killed)
			if tc.dropShm {
				if err := os.Remove(path + "-shm"); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			b := &recoveryTestBudget{rows: 100, bytes: 1 << 20}
			c, err := ReadRecoveryComposer(t.Context(), path, "c", 0, 1<<20, b)
			if err != nil || len(c.Bubbles) != 3 || string(c.Bubbles[0].Value) != bubble("first") || !c.Bubbles[1].Missing {
				t.Fatal(c, err)
			}
			assertUnchanged(t, dir, before)
			assertEmpty(t, root)
		})
	}
}

// TestRecoveryRefusesWALFrames: frames in the -wal, from a running Cursor or
// one that was killed before checkpointing, are never read by recovery
// (immutable would ignore them), and nothing beside the database changes.
func TestRecoveryRefusesWALFrames(t *testing.T) {
	for _, killed := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "killed"}[killed], func(t *testing.T) {
			path := StateDatabase(t.TempDir())
			w := settledWriter(t, path, false)
			w.put(map[string]string{"composerData:late": chat("late", 1, "x")})
			if killed {
				w.kill()
			}
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			err := ReadRecovery(t.Context(), path, &recoveryTestBudget{rows: 100, bytes: 1 << 20}, func(context.Context, *sql.DB) error {
				t.Fatal("read a database with WAL frames")
				return nil
			})
			if ReasonOf(err) != Locked {
				t.Fatal(err)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestRecoverySettledReadInvalidatedByChange: a settled read that sees
// Cursor write, checkpoint, or change its side files while it reads is
// ChangedDuringRead, not a result.
func TestRecoverySettledReadInvalidatedByChange(t *testing.T) {
	for name, tc := range map[string]struct {
		killed bool
		setup  func(t *testing.T, path string)
		change func(t *testing.T, w *writer, path string)
	}{
		"write fills the -wal": {change: func(t *testing.T, w *writer, _ string) {
			t.Helper()
			w.put(map[string]string{"composerData:late": chat("late", 1, "x")})
		}},
		"write checkpointed and truncated": {change: func(t *testing.T, w *writer, _ string) {
			t.Helper()
			w.put(map[string]string{"composerData:late": chat("late", 1, "x")})
			w.do(writerCommand{Op: writerCheckpoint})
		}},
		"-shm removed": {killed: true, change: func(t *testing.T, _ *writer, path string) {
			t.Helper()
			if err := os.Remove(path + "-shm"); err != nil {
				t.Fatal(err)
			}
		}},
		"-shm appears beside a stray -wal": {killed: true, setup: func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path + "-shm"); err != nil {
				t.Fatal(err)
			}
		}, change: func(t *testing.T, _ *writer, path string) {
			t.Helper()
			mustWrite(t, path+"-shm", nil)
		}},
		"journal appears": {killed: true, change: func(t *testing.T, _ *writer, path string) {
			t.Helper()
			mustWrite(t, path+"-journal", nil)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			path := StateDatabase(t.TempDir())
			w := settledWriter(t, path, tc.killed)
			if tc.setup != nil {
				tc.setup(t, path)
			}
			called := false
			err := ReadRecovery(t.Context(), path, &recoveryTestBudget{rows: 100, bytes: 1 << 20}, func(ctx context.Context, db *sql.DB) error {
				called = true
				var n int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM cursorDiskKV`).Scan(&n); err != nil {
					return err
				}
				tc.change(t, w, path)
				return nil
			})
			if !called || ReasonOf(err) != ChangedDuringRead {
				t.Fatal(called, err)
			}
		})
	}
}
