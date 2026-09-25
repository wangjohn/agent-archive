package collector

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// TestCursorSQLiteRememberedFailureWithRequest: a hook request on a chat
// whose read failed at the state it is still in costs no copy of the
// database. A filter failure is reported again and the request stays queued
// (its evidence is the only copy); a size-limit gap completes the request,
// as blocking does. A changed chat is read with the request.
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
	remote := storage.NewMemoryStore()
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
	if _, err := processSession(context.Background(), local, storage.NewMemoryStore(), reg, mustRequest(t, local, reg.ArchiveSessionID), opts.now(), opts); err == nil {
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
