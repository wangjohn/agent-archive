package backfill

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// One chat whose composerData the reader can't decode (a header without a
// message ID) is that chat's unsafe_format; the database's other chats are
// still imported, and the database counts as checked.
func TestCursorDatabaseChatFormatErrorIsThatChats(t *testing.T) {
	tr := newTree(t)
	site := tr.repo("home/site")
	uri := map[string]any{"uri": "file://" + site}
	created := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC).UnixMilli()
	writeCursorDB(t, CursorStateDatabase(tr.home), true, mergeRows(
		chatRows("good", map[string]any{"workspaceIdentifier": uri}, "a", "b"),
		map[string]any{"composerData:odd": composerJSON("odd", 0, map[string]any{
			"createdAt": created, "workspaceIdentifier": uri, "fullConversationHeadersOnly": []any{map[string]any{"type": 1}},
		})},
	))
	env := tr.env()
	env.CursorDatabase = CursorDatabaseReader(tr.home)
	p := plan(t, env, states{}, config.Config{}, Filters{})
	got := map[string]SkipReason{}
	for _, c := range databaseCandidates(p) {
		got[c.NativeSessionID] = c.Skip
	}
	if !p.CursorDatabaseChecked || !reflect.DeepEqual(got, map[string]SkipReason{"good": "", "odd": SkipUnsafeFormat}) {
		t.Fatalf("checked %v (%q), outcomes %v", p.CursorDatabaseChecked, p.CursorDatabaseUnchecked, got)
	}
}

// A failure of the database itself while a chat is read (Cursor held a lock
// past the timeout) leaves the whole database unchecked, with that reason.
func TestCursorDatabaseReadFailureUnchecksTheDatabase(t *testing.T) {
	tr := newTree(t)
	site := tr.repo("home/site")
	chats := []CursorDatabaseChat{{ID: "a", Folder: site, CreatedAt: time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)}, {ID: "b", Folder: site, CreatedAt: time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)}}
	env := tr.env()
	env.CursorDatabase = func(ctx context.Context) (CursorDatabaseResult, error) {
		res, _ := fakeCursorDatabase(chats, map[string]cursorstore.Composer{"a": syntheticChat("a", nil, "x")}, nil, nil)(ctx)
		read := res.ReadChat
		res.ReadChat = func(ctx context.Context, id string) (cursorstore.Composer, error) {
			if id == "b" {
				return cursorstore.Composer{}, cursorstore.NotChecked(cursorstore.Locked)
			}
			return read(ctx, id)
		}
		return res, nil
	}
	p := plan(t, env, states{}, config.Config{}, Filters{})
	if p.CursorDatabaseChecked || p.CursorDatabaseUnchecked != CursorUncheckedLocked || len(databaseCandidates(p)) != 0 {
		t.Fatalf("checked %v, reason %q, %d candidates", p.CursorDatabaseChecked, p.CursorDatabaseUnchecked, len(databaseCandidates(p)))
	}
}

// Two rows naming one chat: the row whose key is the chat's ID is the one
// kept, whichever comes first, so a stray row can't make the real chat an
// identity_mismatch.
func TestCursorDatabaseDuplicatePrefersTheMatchingKey(t *testing.T) {
	tr := newTree(t)
	site := tr.repo("home/site")
	at := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
	for _, order := range [][]CursorDatabaseChat{
		{{ID: "x", KeyID: "stray", Folder: site, CreatedAt: at}, {ID: "x", KeyID: "x", Folder: site, CreatedAt: at}},
		{{ID: "x", KeyID: "x", Folder: site, CreatedAt: at}, {ID: "x", KeyID: "stray", Folder: site, CreatedAt: at}},
	} {
		env := tr.env()
		env.CursorDatabase = fakeCursorDatabase(order, map[string]cursorstore.Composer{"x": syntheticChat("x", nil, "hi")}, nil, nil)
		p := plan(t, env, states{}, config.Config{}, Filters{})
		if got := databaseOutcomes(p); !reflect.DeepEqual(got, map[SkipReason]int{"": 1, SkipDuplicateSession: 1}) {
			t.Fatalf("keys %s then %s: %v", order[0].KeyID, order[1].KeyID, got)
		}
	}
}

// Ctrl-C while the plan reads chats cancels its context: planning stops with
// the cancellation, and the plan's copy of the database is still removed.
func TestCursorDatabaseSnapshotClosedWhenPlanningIsCancelled(t *testing.T) {
	tr := newTree(t)
	site := tr.repo("home/site")
	at := time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)
	chats := []CursorDatabaseChat{{ID: "a", Folder: site, CreatedAt: at}, {ID: "b", Folder: site, CreatedAt: at}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := false
	env := tr.env()
	env.CursorDatabase = func(ctx context.Context) (CursorDatabaseResult, error) {
		res, _ := fakeCursorDatabase(chats, map[string]cursorstore.Composer{"a": syntheticChat("a", nil, "x"), "b": syntheticChat("b", nil, "y")}, nil, nil)(ctx)
		read := res.ReadChat
		res.ReadChat = func(ctx context.Context, id string) (cursorstore.Composer, error) {
			cancel() // Ctrl-C arrives mid-read.
			if err := ctx.Err(); err != nil {
				return cursorstore.Composer{}, err
			}
			return read(ctx, id)
		}
		res.Close = func() error { closed = true; return nil }
		return res, nil
	}
	_, err := BuildPlan(ctx, env, states{}, config.Config{}, Filters{})
	if !errors.Is(err, context.Canceled) || !closed {
		t.Fatalf("err %v, snapshot closed %v", err, closed)
	}
}

// The plan's copy of the database that can't be removed fails the plan,
// rather than being left silently in the temporary folder.
func TestCursorDatabaseCloseErrorIsReported(t *testing.T) {
	tr := newTree(t)
	env := tr.env()
	failed := errors.New("remove the Cursor database snapshot")
	env.CursorDatabase = func(ctx context.Context) (CursorDatabaseResult, error) {
		res, _ := fakeCursorDatabase(nil, nil, nil, nil)(ctx)
		res.Close = func() error { return failed }
		return res, nil
	}
	if _, err := BuildPlan(context.Background(), env, states{}, config.Config{}, Filters{}); !errors.Is(err, failed) {
		t.Fatalf("err %v", err)
	}
}
