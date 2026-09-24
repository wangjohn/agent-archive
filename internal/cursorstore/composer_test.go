package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wantComposer is the Composer a read of rows should return for id.
func wantComposer(rows map[string]string, id string, bubbles ...string) Composer {
	c := Composer{Composer: json.RawMessage(rows["composerData:"+id]), Bubbles: []Bubble{}}
	for _, b := range bubbles {
		var v json.RawMessage
		if s, ok := rows["bubbleId:"+id+":"+b]; ok {
			v = json.RawMessage(s)
		}
		c.Bubbles = append(c.Bubbles, Bubble{ID: b, Value: v})
	}
	return c
}

func assertComposer(t *testing.T, got, want Composer) {
	t.Helper()
	if string(got.Composer) != string(want.Composer) {
		t.Fatalf("composer %s, want %s", got.Composer, want.Composer)
	}
	if len(got.Bubbles) != len(want.Bubbles) {
		t.Fatalf("%d bubbles, want %d", len(got.Bubbles), len(want.Bubbles))
	}
	for i := range want.Bubbles {
		g, w := got.Bubbles[i], want.Bubbles[i]
		if g.ID != w.ID || string(g.Value) != string(w.Value) || (g.Value == nil) != (w.Value == nil) {
			t.Fatalf("bubble %d: %s=%s, want %s=%s", i, g.ID, g.Value, w.ID, w.Value)
		}
	}
}

// chatRows is one chat with three messages, the second row missing, beside
// rows the read must not return: another chat whose ID extends this one's,
// a message of this chat no header lists, and an unrelated key.
func chatRows() map[string]string {
	return map[string]string{
		"composerData:c":   chat("c", 1000, "b1", "b2", "b3"),
		"bubbleId:c:b1":    bubble("first"),
		"bubbleId:c:b3":    bubble("third"),
		"bubbleId:c:extra": bubble("not listed"),
		"composerData:c2":  chat("c2", 1, "x"),
		"bubbleId:c2:x":    bubble("other chat"),
		"bubbleId:c2:b1":   bubble("other chat, same bubble ID"),
		"composerDataZ":    "unrelated",
	}
}

func toAny(rows map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range rows {
		out[k] = v
	}
	return out
}

// TestReadComposerClosed reads a chat with Cursor closed: in place,
// immutable, with no snapshot, in both journal modes.
func TestReadComposerClosed(t *testing.T) {
	for _, wal := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "wal"}[wal], func(t *testing.T) {
			path := StateDatabase(t.TempDir())
			rows := chatRows()
			writeDB(t, path, wal, toAny(rows))
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			if len(before) != 2 {
				t.Fatalf("side files before the read: %v", before)
			}
			scratch := filepath.Join(t.TempDir(), "scratch")
			afterSnapshot = func(string) { t.Error("Cursor closed, yet the database was copied") }
			defer func() { afterSnapshot = nil }()

			c, sig, err := ReadComposer(context.Background(), path, "c", scratch)
			if err != nil {
				t.Fatal(err)
			}
			assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3"))
			if c.Bubbles[1].Value != nil {
				t.Fatal("a missing message row has a value")
			}
			if want := (Signature{LastUpdatedAt: 1000, HeaderCount: 3, LastBubbleID: "b3"}); sig != want {
				t.Fatalf("signature %+v, want %+v", sig, want)
			}
			assertUnchanged(t, dir, before)
			assertEmpty(t, scratch)
		})
	}
}

// TestReadComposerLive reads a chat while Cursor holds the database open
// with writes only its -wal holds, and one transaction still open: the read
// is a snapshot of the committed state, taken with the backup API into a
// private 0600 file that is gone afterwards.
func TestReadComposerLive(t *testing.T) {
	path := StateDatabase(t.TempDir())
	w := startWriter(t, path)
	rows := chatRows()
	w.put(rows)
	dir := filepath.Dir(path)
	scratch := t.TempDir()

	var copies []string
	afterSnapshot = func(copyPath string) {
		copies = append(copies, copyPath)
		info, err := os.Stat(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("snapshot mode %v, want 0600", info.Mode().Perm())
		}
		parent, err := os.Stat(filepath.Dir(copyPath))
		if err != nil {
			t.Fatal(err)
		}
		if parent.Mode().Perm() != 0o700 || filepath.Dir(filepath.Dir(copyPath)) != scratch {
			t.Errorf("snapshot directory %s mode %v, want 0700 under %s", filepath.Dir(copyPath), parent.Mode().Perm(), scratch)
		}
		if !strings.HasPrefix(filepath.Base(filepath.Dir(copyPath)), snapshotPrefix) {
			t.Errorf("snapshot directory %s", filepath.Dir(copyPath))
		}
	}
	defer func() { afterSnapshot = nil }()

	read := func() (Composer, Signature) {
		t.Helper()
		before := snapshotDir(t, dir)
		if _, ok := before["state.vscdb-wal"]; !ok {
			t.Fatal("the writer has no -wal file")
		}
		c, sig, err := ReadComposer(context.Background(), path, "c", scratch)
		if err != nil {
			t.Fatal(err)
		}
		assertUnchanged(t, dir, before)
		assertEmpty(t, scratch)
		return c, sig
	}

	c, sig := read()
	assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3"))
	if want := (Signature{LastUpdatedAt: 1000, HeaderCount: 3, LastBubbleID: "b3"}); sig != want {
		t.Fatalf("signature %+v, want %+v", sig, want)
	}
	if len(copies) != 1 {
		t.Fatalf("%d snapshots, want 1", len(copies))
	}

	// Cursor starts a write and has not committed it: the snapshot is the
	// committed state, the new message and its row together or neither.
	w.do(writerCommand{Op: "begin"},
		writerCommand{Op: "put", Key: "bubbleId:c:b2", Value: bubble("second")},
		writerCommand{Op: "put", Key: "composerData:c", Value: chat("c", 2000, "b1", "b2", "b3", "b4")},
		writerCommand{Op: "put", Key: "bubbleId:c:b4", Value: bubble("fourth")})
	c, sig = read()
	assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3"))
	if sig.HeaderCount != 3 {
		t.Fatalf("an uncommitted write was read: %+v", sig)
	}

	w.do(writerCommand{Op: "commit"})
	rows["bubbleId:c:b2"] = bubble("second")
	rows["composerData:c"] = chat("c", 2000, "b1", "b2", "b3", "b4")
	rows["bubbleId:c:b4"] = bubble("fourth")
	c, sig = read()
	assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3", "b4"))
	if want := (Signature{LastUpdatedAt: 2000, HeaderCount: 4, LastBubbleID: "b4"}); sig != want {
		t.Fatalf("signature %+v, want %+v", sig, want)
	}
}

// TestReadComposerSignature: the signature changes exactly when the chat
// does, with Cursor running and closed, and ReadSignature agrees with
// ReadComposer.
func TestReadComposerSignature(t *testing.T) {
	type step struct {
		name    string
		rows    map[string]string
		changed bool
	}
	steps := []step{
		{"another chat changes", map[string]string{"composerData:c2": chat("c2", 5, "x", "y"), "bubbleId:c2:y": bubble("y")}, false},
		{"an unrelated key", map[string]string{"composerDataZ": "changed", "bubbleId:other:1": bubble("o")}, false},
		{"a message nobody lists", map[string]string{"bubbleId:c:extra": bubble("still not listed")}, false},
		{"the chat is rewritten unchanged", map[string]string{"composerData:c": chat("c", 1000, "b1", "b2", "b3")}, false},
		{"a new message", map[string]string{"composerData:c": chat("c", 1000, "b1", "b2", "b3", "b4"), "bubbleId:c:b4": bubble("four")}, true},
		{"lastUpdatedAt", map[string]string{"composerData:c": chat("c", 3000, "b1", "b2", "b3", "b4")}, true},
		{"the last message replaced", map[string]string{"composerData:c": chat("c", 3000, "b1", "b2", "b3", "b5")}, true},
	}
	check := func(t *testing.T, path, scratch string, apply func(map[string]string)) {
		_, prev, err := ReadComposer(context.Background(), path, "c", scratch)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			apply(s.rows)
			_, sig, err := ReadComposer(context.Background(), path, "c", scratch)
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			only, err := ReadSignature(context.Background(), path, "c")
			if err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if only != sig {
				t.Fatalf("%s: ReadSignature %+v, ReadComposer %+v", s.name, only, sig)
			}
			if (sig != prev) != s.changed {
				t.Fatalf("%s: signature %+v -> %+v, want changed=%v", s.name, prev, sig, s.changed)
			}
			prev = sig
		}
		assertEmpty(t, scratch)
	}
	t.Run("closed", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		writeDB(t, path, true, toAny(chatRows()))
		check(t, path, t.TempDir(), func(rows map[string]string) { writeDB(t, path, true, toAny(rows)) })
	})
	t.Run("running", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		w := startWriter(t, path)
		w.put(chatRows())
		check(t, path, t.TempDir(), w.put)
	})
}

// TestReadComposerOldChat: an older chat keeps its messages inline, so there
// are no message rows to read.
func TestReadComposerOldChat(t *testing.T) {
	path := StateDatabase(t.TempDir())
	value := `{"_v":2,"composerId":"old","lastUpdatedAt":5.0,"conversation":[{"bubbleId":"i1","text":"a"},{"bubbleId":"i2","text":"b"}]}`
	writeDB(t, path, false, map[string]any{"composerData:old": []byte(value), "bubbleId:old:i1": bubble("row")})
	c, sig, err := ReadComposer(context.Background(), path, "old", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Composer) != value || len(c.Bubbles) != 0 {
		t.Fatalf("%+v", c)
	}
	if want := (Signature{LastUpdatedAt: 5, HeaderCount: 2, LastBubbleID: "i2"}); sig != want {
		t.Fatalf("signature %+v, want %+v", sig, want)
	}
}

// TestReadComposerErrors: nothing that can't be read safely is returned, and
// no snapshot outlives a failed read, including one that panics.
func TestReadComposerErrors(t *testing.T) {
	t.Run("no database", func(t *testing.T) {
		scratch := t.TempDir()
		_, _, err := ReadComposer(context.Background(), StateDatabase(t.TempDir()), "c", scratch)
		if !errors.Is(err, ErrNoDatabase) || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err %v", err)
		}
		if _, err := ReadSignature(context.Background(), StateDatabase(t.TempDir()), "c"); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("err %v", err)
		}
		assertEmpty(t, scratch)
	})
	t.Run("no chat id", func(t *testing.T) {
		if _, _, err := ReadComposer(context.Background(), "x", "", t.TempDir()); err == nil {
			t.Fatal("read with no composer ID")
		}
	})
	t.Run("journal", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		writeDB(t, path, false, toAny(chatRows()))
		if err := os.WriteFile(path+"-journal", []byte("journal"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(path)
		before := snapshotDir(t, dir)
		_, _, err := ReadComposer(context.Background(), path, "c", t.TempDir())
		var nc *NotCheckedError
		if !errors.As(err, &nc) || nc.Reason != Locked {
			t.Fatalf("err %v", err)
		}
		assertUnchanged(t, dir, before)
	})
	t.Run("changed during read", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		writeDB(t, path, true, toAny(chatRows()))
		// ReadComposer with Cursor closed is Read's checked immutable read.
		err := Read(context.Background(), path, Options{AfterImmutableRead: func(p string) {
			later := time.Now().Add(time.Minute)
			os.Chtimes(p, later, later)
		}}, func(ctx context.Context, db *sql.DB) error {
			_, _, err := queryComposer(ctx, db, "c")
			return err
		})
		if ReasonOf(err) != ChangedDuringRead {
			t.Fatalf("err %v", err)
		}
	})

	for name, tc := range map[string]struct {
		rows   map[string]string
		id     string
		panics bool
		check  func(error) bool
	}{
		"missing chat":         {chatRows(), "nope", false, func(err error) bool { return errors.Is(err, ErrComposerNotFound) && errors.Is(err, fs.ErrNotExist) }},
		"not json":             {map[string]string{"composerData:c": "not json"}, "c", false, isReason(UnknownFormat)},
		"header without an ID": {map[string]string{"composerData:c": `{"fullConversationHeadersOnly":[{"type":1}]}`}, "c", false, isReason(UnknownFormat)},
		"headers not a list":   {map[string]string{"composerData:c": `{"fullConversationHeadersOnly":{"a":1}}`}, "c", false, isReason(UnknownFormat)},
		"panic while reading":  {chatRows(), "c", true, nil},
	} {
		t.Run(name, func(t *testing.T) {
			for _, running := range []bool{false, true} {
				path := StateDatabase(t.TempDir())
				if running {
					startWriter(t, path).put(tc.rows)
				} else {
					writeDB(t, path, true, toAny(tc.rows))
				}
				dir := filepath.Dir(path)
				before := snapshotDir(t, dir)
				scratch := t.TempDir()
				if tc.panics {
					if !running {
						continue
					}
					afterSnapshot = func(copyPath string) {
						if _, err := os.Stat(copyPath); err != nil {
							t.Errorf("no snapshot: %v", err)
						}
						panic("reading the snapshot failed")
					}
					func() {
						defer func() {
							if recover() == nil {
								t.Error("no panic")
							}
						}()
						ReadComposer(context.Background(), path, tc.id, scratch)
					}()
					afterSnapshot = nil
				} else {
					c, sig, err := ReadComposer(context.Background(), path, tc.id, scratch)
					if !tc.check(err) || c.Composer != nil || sig != (Signature{}) {
						t.Fatalf("running=%v: err %v, %+v %+v", running, err, c, sig)
					}
				}
				assertUnchanged(t, dir, before)
				assertEmpty(t, scratch)
			}
		})
	}
}

func isReason(r Reason) func(error) bool {
	return func(err error) bool {
		var nc *NotCheckedError
		return errors.As(err, &nc) && nc.Reason == r
	}
}

// TestReadComposerRemovesStaleSnapshots: a snapshot a killed process left
// behind is removed by the next live read once it is old; a recent one,
// which may be another reader's, is left alone.
func TestReadComposerRemovesStaleSnapshots(t *testing.T) {
	path := StateDatabase(t.TempDir())
	startWriter(t, path).put(chatRows())
	scratch := t.TempDir()
	stale := filepath.Join(scratch, snapshotPrefix+"stale")
	recent := filepath.Join(scratch, snapshotPrefix+"recent")
	other := filepath.Join(scratch, "not-a-snapshot")
	for _, d := range []string{stale, recent, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "state.vscdb"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleSnapshotAge)
	for _, d := range []string{stale, other} {
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ReadComposer(context.Background(), path, "c", scratch); err != nil {
		t.Fatal(err)
	}
	for d, want := range map[string]bool{stale: false, recent: true, other: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(d), err == nil, want)
		}
	}
}

// TestBubbleQueryUsesIndex: a chat's messages are read through the key
// index, not a scan of every row.
func TestBubbleQueryUsesIndex(t *testing.T) {
	db := openWriter(t, filepath.Join(t.TempDir(), "state.vscdb"), false)
	defer db.Close()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+bubbleQuery, "bubbleId:c:", "bubbleId:c;")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if got := strings.Join(plan, "; "); !strings.Contains(got, "USING INDEX") {
		t.Fatalf("query plan %q does not search the key index", got)
	}
}
