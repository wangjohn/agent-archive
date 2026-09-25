package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
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

// chatSignature is chatRows' Signature for chat c.
func chatSignature() Signature {
	return Signature{LastUpdatedAt: 1000, HeaderCount: 3, LastBubbleID: "b3", MessageRows: 2, LastMessageHash: messageHash([]byte(bubble("third")))}
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
			root := useTempSnapshots(t)
			path := StateDatabase(t.TempDir())
			rows := chatRows()
			writeDB(t, path, wal, toAny(rows))
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			if len(before) != 2 {
				t.Fatalf("side files before the read: %v", before)
			}
			r := NewReader(path)
			defer closeOrFail(t, r)
			c, sig, err := r.ReadComposer(context.Background(), "c")
			if err != nil {
				t.Fatal(err)
			}
			assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3"))
			if c.Bubbles[1].Value != nil {
				t.Fatal("a missing message row has a value")
			}
			if sig != chatSignature() {
				t.Fatalf("signature %+v, want %+v", sig, chatSignature())
			}
			if r.Snapshots() != 0 {
				t.Fatal("Cursor closed, yet the database was copied")
			}
			assertUnchanged(t, dir, before)
			assertEmpty(t, root)
		})
	}
}

// TestReadComposerLive reads a chat while Cursor holds the database open
// with writes only its -wal holds, and one transaction still open: the read
// is a snapshot of the committed state, taken with the backup API into a
// private 0600 file in the per-user temporary directory, gone afterwards.
func TestReadComposerLive(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	w := startWriter(t, path)
	rows := chatRows()
	w.put(rows)
	dir := filepath.Dir(path)

	var copies []string
	hooks := readerHooks{afterSnapshot: func(copyPath string) {
		copies = append(copies, copyPath)
		info, err := os.Stat(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("snapshot mode %v, want 0600", info.Mode().Perm())
		}
		for _, d := range []string{filepath.Dir(copyPath), root} {
			info, err := os.Stat(d)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Errorf("%s mode %v, want 0700", d, info.Mode().Perm())
			}
		}
		if filepath.Dir(filepath.Dir(copyPath)) != root || !strings.HasPrefix(filepath.Base(filepath.Dir(copyPath)), snapshotPrefix) {
			t.Errorf("snapshot at %s, want under %s", copyPath, root)
		}
	}}

	read := func() (Composer, Signature) {
		t.Helper()
		before := snapshotDir(t, dir)
		if _, ok := before["state.vscdb-wal"]; !ok {
			t.Fatal("the writer has no -wal file")
		}
		c, sig, err := readComposerWith(context.Background(), path, "c", hooks)
		if err != nil {
			t.Fatal(err)
		}
		assertUnchanged(t, dir, before)
		assertEmpty(t, root)
		return c, sig
	}

	c, sig := read()
	assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3"))
	if sig != chatSignature() {
		t.Fatalf("signature %+v, want %+v", sig, chatSignature())
	}
	if len(copies) != 1 {
		t.Fatalf("%d snapshots, want 1", len(copies))
	}

	// Cursor starts a write and has not committed it: the snapshot is the
	// committed state, the new message and its row together or neither.
	w.do(writerCommand{Op: writerBegin},
		writerCommand{Op: writerPut, Key: "bubbleId:c:b2", Value: bubble("second")},
		writerCommand{Op: writerPut, Key: "composerData:c", Value: chat("c", 2000, "b1", "b2", "b3", "b4")},
		writerCommand{Op: writerPut, Key: "bubbleId:c:b4", Value: bubble("fourth")})
	c, sig = read()
	assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3"))
	if sig != chatSignature() {
		t.Fatalf("an uncommitted write was read: %+v", sig)
	}

	w.do(writerCommand{Op: writerCommit})
	rows["bubbleId:c:b2"] = bubble("second")
	rows["composerData:c"] = chat("c", 2000, "b1", "b2", "b3", "b4")
	rows["bubbleId:c:b4"] = bubble("fourth")
	c, sig = read()
	assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3", "b4"))
	if want := (Signature{LastUpdatedAt: 2000, HeaderCount: 4, LastBubbleID: "b4", MessageRows: 4, LastMessageHash: messageHash([]byte(bubble("fourth")))}); sig != want {
		t.Fatalf("signature %+v, want %+v", sig, want)
	}
}

// TestReaderTakesOneSnapshot: one Reader reads any number of chats while
// Cursor runs from a single copy, removed by Close; with Cursor closed it
// copies nothing.
func TestReaderTakesOneSnapshot(t *testing.T) {
	root := useTempSnapshots(t)
	rows := map[string]string{}
	var ids []string
	for i := range 5 {
		id := fmt.Sprintf("chat-%d", i)
		ids = append(ids, id)
		rows["composerData:"+id] = chat(id, int64(i+1), "m")
		rows["bubbleId:"+id+":m"] = bubble(id)
	}

	t.Run("running", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		startWriter(t, path).put(rows)
		r := NewReader(path)
		for _, id := range ids {
			c, _, err := r.ReadComposer(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			assertComposer(t, c, wantComposer(rows, id, "m"))
		}
		if r.Snapshots() != 1 {
			t.Fatalf("%d snapshots for %d chats, want 1", r.Snapshots(), len(ids))
		}
		if entries, _ := os.ReadDir(root); len(entries) != 1 {
			t.Fatalf("snapshot directories while open: %v", entries)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		assertEmpty(t, root)
		// Usable again after Close: a later read takes a fresh copy.
		if _, _, err := r.ReadComposer(context.Background(), ids[0]); err != nil || r.Snapshots() != 2 {
			t.Fatalf("after Close: %v, %d snapshots", err, r.Snapshots())
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		assertEmpty(t, root)
	})
	t.Run("closed", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		writeDB(t, path, true, toAny(rows))
		r := NewReader(path)
		defer closeOrFail(t, r)
		for _, id := range ids {
			if _, _, err := r.ReadComposer(context.Background(), id); err != nil {
				t.Fatal(err)
			}
		}
		if r.Snapshots() != 0 {
			t.Fatalf("%d snapshots with Cursor closed", r.Snapshots())
		}
	})
}

// TestReadComposerSignature: the signature changes exactly when the chat
// does, with Cursor running and closed, and ReadSignature agrees with
// ReadComposer.
func TestReadComposerSignature(t *testing.T) {
	useTempSnapshots(t)
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
		{"a missing row arrives", map[string]string{"bubbleId:c:b2": bubble("second, late")}, true},
		{"the last message is edited", map[string]string{"bubbleId:c:b3": bubble("third, still streaming")}, true},
		{"a new message", map[string]string{"composerData:c": chat("c", 1000, "b1", "b2", "b3", "b4")}, true},
		{"its row arrives later", map[string]string{"bubbleId:c:b4": bubble("four")}, true},
		{"lastUpdatedAt", map[string]string{"composerData:c": chat("c", 3000, "b1", "b2", "b3", "b4")}, true},
		{"the last message replaced", map[string]string{"composerData:c": chat("c", 3000, "b1", "b2", "b3", "b5")}, true},
	}
	check := func(t *testing.T, path string, apply func(map[string]string)) {
		t.Helper()
		_, prev, err := ReadComposer(context.Background(), path, "c")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			apply(s.rows)
			_, sig, err := ReadComposer(context.Background(), path, "c")
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
	}
	t.Run("closed", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		writeDB(t, path, true, toAny(chatRows()))
		check(t, path, func(rows map[string]string) { writeDB(t, path, true, toAny(rows)) })
	})
	t.Run("running", func(t *testing.T) {
		path := StateDatabase(t.TempDir())
		w := startWriter(t, path)
		w.put(chatRows())
		check(t, path, w.put)
	})
}

// TestReadComposerOldChat: an older chat keeps its messages inline, so there
// are no message rows to read.
func TestReadComposerOldChat(t *testing.T) {
	path := StateDatabase(t.TempDir())
	value := `{"_v":2,"composerId":"old","lastUpdatedAt":5.0,"conversation":[{"bubbleId":"i1","text":"a"},{"bubbleId":"i2","text":"b"}]}`
	writeDB(t, path, false, map[string]any{"composerData:old": []byte(value), "bubbleId:old:i1": bubble("row")})
	c, sig, err := ReadComposer(context.Background(), path, "old")
	if err != nil {
		t.Fatal(err)
	}
	if string(c.Composer) != value || len(c.Bubbles) != 0 {
		t.Fatalf("%+v", c)
	}
	if want := (Signature{LastUpdatedAt: 5, HeaderCount: 2, LastBubbleID: "i2"}); sig != want {
		t.Fatalf("signature %+v, want %+v", sig, want)
	}
	if only, err := ReadSignature(context.Background(), path, "old"); err != nil || only != sig {
		t.Fatalf("ReadSignature %+v, %v", only, err)
	}
}

// TestStrayEmptyWAL: the empty -wal SQLite can leave when Cursor quits just
// as a reader opens the database has nothing to replay, so the database is
// read as closed, and left as it was. A -wal with frames and no -shm is
// still not read.
func TestStrayEmptyWAL(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	rows := chatRows()
	writeDB(t, path, true, toAny(rows))
	if err := os.WriteFile(path+"-wal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	c, sig, err := ReadComposer(context.Background(), path, "c")
	if err != nil {
		t.Fatal(err)
	}
	assertComposer(t, c, wantComposer(rows, "c", "b1", "b2", "b3"))
	if only, err := ReadSignature(context.Background(), path, "c"); err != nil || only != sig {
		t.Fatalf("ReadSignature %+v, %v", only, err)
	}
	assertUnchanged(t, dir, before)
	assertEmpty(t, root)

	// Cursor starts during the read: the -wal grows or a -shm appears.
	for name, change := range map[string]func(string){
		"wal grew":    func(p string) { mustWrite(t, p+"-wal", []byte("frames")) },
		"shm appears": func(p string) { mustWrite(t, p+"-shm", nil) },
	} {
		err := Read(context.Background(), path, Options{AfterImmutableRead: change}, func(context.Context, *sql.DB) error { return nil })
		if ReasonOf(err) != ChangedDuringRead {
			t.Fatalf("%s: err %v", name, err)
		}
		mustWrite(t, path+"-wal", nil)
		if err := os.Remove(path + "-shm"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(path+"-wal", []byte("frames"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadComposer(context.Background(), path, "c"); ReasonOf(err) != Unreadable {
		t.Fatalf("a -wal with frames and no -shm: %v", err)
	}
}

// TestReadComposerErrors: nothing that can't be read safely is returned, no
// snapshot outlives a failed read, including one that panics, and no error
// names the database's path.
func TestReadComposerErrors(t *testing.T) {
	root := useTempSnapshots(t)
	t.Run("no database", func(t *testing.T) {
		_, _, err := ReadComposer(context.Background(), StateDatabase(t.TempDir()), "c")
		if !errors.Is(err, ErrNoDatabase) || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err %v", err)
		}
		if _, err := ReadSignature(context.Background(), StateDatabase(t.TempDir()), "c"); !errors.Is(err, ErrNoDatabase) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("no chat id", func(t *testing.T) {
		if _, _, err := ReadComposer(context.Background(), "x", ""); err == nil {
			t.Fatal("read with no composer ID")
		}
	})
	t.Run("the path is never in an error", func(t *testing.T) {
		secret := filepath.Join(t.TempDir(), "secret-folder-name")
		if err := os.WriteFile(secret, []byte("a file, not a folder"), 0o600); err != nil {
			t.Fatal(err)
		}
		garbage := filepath.Join(t.TempDir(), "secret-garbage.vscdb")
		if err := os.WriteFile(garbage, []byte(strings.Repeat("SQLite format 3\x00", 20)), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{filepath.Join(secret, "state.vscdb"), garbage} {
			_, _, err := ReadComposer(context.Background(), p, "c")
			_, sigErr := ReadSignature(context.Background(), p, "c")
			for _, e := range []error{err, sigErr} {
				if e == nil || strings.Contains(e.Error(), "secret") {
					t.Fatalf("error %v", e)
				}
			}
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
		_, _, err := ReadComposer(context.Background(), path, "c")
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
			if err := os.Chtimes(p, later, later); err != nil {
				t.Error(err)
			}
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
				if tc.panics {
					if !running {
						continue
					}
					panicking := readerHooks{afterSnapshot: func(copyPath string) {
						if _, err := os.Stat(copyPath); err != nil {
							t.Errorf("no snapshot: %v", err)
						}
						panic("reading the snapshot failed")
					}}
					func() {
						defer func() {
							if recover() == nil {
								t.Error("no panic")
							}
						}()
						_, _, _ = readComposerWith(context.Background(), path, tc.id, panicking) // expected to panic
					}()
				} else {
					c, sig, err := ReadComposer(context.Background(), path, tc.id)
					if !tc.check(err) || c.Composer != nil || sig != (Signature{}) {
						t.Fatalf("running=%v: err %v, %+v %+v", running, err, c, sig)
					}
					if _, err := ReadSignature(context.Background(), path, tc.id); !tc.check(err) {
						t.Fatalf("running=%v: ReadSignature err %v", running, err)
					}
				}
				assertUnchanged(t, dir, before)
				assertEmpty(t, root)
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

// TestSnapshotRootMustBePrivate: no copy is written into a snapshot
// directory another user could open, or one that is a link elsewhere.
func TestSnapshotRootMustBePrivate(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, root string){
		"readable by others": func(t *testing.T, root string) {
			t.Helper()
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"a link": func(t *testing.T, root string) {
			t.Helper()
			if err := os.Symlink(t.TempDir(), root); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := useTempSnapshots(t)
			setup(t, root)
			path := StateDatabase(t.TempDir())
			startWriter(t, path).put(chatRows())
			if _, _, err := ReadComposer(context.Background(), path, "c"); !errors.Is(err, errSnapshotRootNotPrivate) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

// TestStaleSnapshotsAreRemoved: a snapshot a killed process left behind is
// removed by the next read, with Cursor running or closed, once it is old; a
// recent one, which may be another reader's, is left alone.
func TestStaleSnapshotsAreRemoved(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			root := useTempSnapshots(t)
			path := StateDatabase(t.TempDir())
			if running {
				startWriter(t, path).put(chatRows())
			} else {
				writeDB(t, path, true, toAny(chatRows()))
			}
			if _, err := SnapshotRoot(); err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(root, snapshotPrefix+"stale")
			recent := filepath.Join(root, snapshotPrefix+"recent")
			other := filepath.Join(root, "not-a-snapshot")
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
			if _, _, err := ReadComposer(context.Background(), path, "c"); err != nil {
				t.Fatal(err)
			}
			for d, want := range map[string]bool{stale: false, recent: true, other: true} {
				if _, err := os.Stat(d); (err == nil) != want {
					t.Errorf("%s exists=%v, want %v", filepath.Base(d), err == nil, want)
				}
			}
		})
	}
}

// TestBackupRetriesWhileBusy: Cursor holding the database's exclusive lock
// makes the backup busy. It is retried until the lock is released, and
// fails as locked, copying nothing, if the lock outlasts the read.
func TestBackupRetriesWhileBusy(t *testing.T) {
	root := useTempSnapshots(t)
	path := StateDatabase(t.TempDir())
	w := startWriter(t, path)
	w.put(chatRows())
	// The lock outlasts the read's deadline.
	w.do(writerCommand{Op: writerExclusive})
	retries := 0
	counting := readerHooks{backupRetried: func() { retries++ }}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	_, _, err := readComposerWith(ctx, path, "c", counting)
	cancel()
	if !isReason(Locked)(err) || retries == 0 {
		t.Fatalf("err %v after %d retries", err, retries)
	}
	assertEmpty(t, root)

	// The lock is released after the first busy step.
	retries = 0
	released := make(chan struct{})
	releasing := readerHooks{backupRetried: func() {
		retries++
		if retries == 1 {
			w.do(writerCommand{Op: writerNormal})
			close(released)
		}
	}}
	c, _, err := readComposerWith(context.Background(), path, "c", releasing)
	if err != nil {
		t.Fatal(err)
	}
	<-released
	assertComposer(t, c, wantComposer(chatRows(), "c", "b1", "b2", "b3"))
	assertEmpty(t, root)
}

// TestBubbleQueryUsesIndex: a chat's messages are read through the key
// index, not a scan of every row.
func TestBubbleQueryUsesIndex(t *testing.T) {
	db := openWriter(t, filepath.Join(t.TempDir(), "state.vscdb"), false)
	defer closeOrFail(t, db)
	for _, q := range []string{bubbleQuery, bubbleKeyQuery} {
		got := queryPlan(t, db, q)
		if !strings.Contains(got, "USING INDEX") && !strings.Contains(got, "USING COVERING INDEX") {
			t.Fatalf("query plan %q does not search the key index", got)
		}
		if q == bubbleKeyQuery && !strings.Contains(got, "USING COVERING INDEX") {
			// Keys alone come from the index, with no lookup of each row.
			t.Fatalf("query plan %q reads the table for keys", got)
		}
	}
}

// queryPlan is SQLite's plan for a bubble query of chat "c", its steps joined
// with "; ".
func queryPlan(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+q, "bubbleId:c:", "bubbleId:c;")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "; ")
}
