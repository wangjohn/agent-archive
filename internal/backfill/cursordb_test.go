package backfill

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// composerJSON builds a composerData value. headers is the header count;
// extra fields are merged in.
func composerJSON(id string, headers int, extra map[string]any) string {
	v := map[string]any{"_v": 3, "composerId": id, "fullConversationHeadersOnly": make([]map[string]any, 0, headers)}
	hs := make([]map[string]any, headers)
	for i := range hs {
		hs[i] = map[string]any{"bubbleId": "m", "type": 1}
	}
	v["fullConversationHeadersOnly"] = hs
	for k, x := range extra {
		v[k] = x
	}
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func millis(t time.Time) int64 { return t.UnixMilli() }

// writeCursorDB creates a synthetic state.vscdb at path, in WAL mode when
// wal is set, and closes it, so SQLite removes its -wal and -shm files.
func writeCursorDB(t *testing.T, path string, wal bool, rows map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db := openCursorWriter(t, path, wal)
	insertCursorRows(t, db, rows)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func openCursorWriter(t *testing.T, path string, wal bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	stmts := []string{`CREATE TABLE IF NOT EXISTS ItemTable (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`,
		`CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`}
	if wal {
		stmts = append([]string{`PRAGMA journal_mode=WAL`}, stmts...)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func insertCursorRows(t *testing.T, db *sql.DB, rows map[string]any) {
	t.Helper()
	for k, v := range rows {
		if _, err := db.Exec(`INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

type fileState struct {
	size    int64
	mode    os.FileMode
	modTime time.Time
	sum     [32]byte
}

// snapshotDir records every file in dir, and dir itself, byte for byte.
func snapshotDir(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	out["."] = fileState{mode: info.Mode(), modTime: info.ModTime()}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		st := fileState{size: info.Size(), mode: info.Mode(), modTime: info.ModTime()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			st.sum = sha256.Sum256(data)
		}
		out[e.Name()] = st
	}
	return out
}

func assertUnchanged(t *testing.T, dir string, before map[string]fileState) {
	t.Helper()
	after := snapshotDir(t, dir)
	for name, a := range after {
		if b, ok := before[name]; !ok {
			t.Errorf("%s was created", name)
		} else if !reflect.DeepEqual(a, b) {
			t.Errorf("%s changed: %+v -> %+v", name, b, a)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("%s was removed", name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

func TestCursorDatabaseReader(t *testing.T) {
	home := t.TempDir()
	path := CursorStateDatabase(home)
	sept10 := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	writeCursorDB(t, path, false, map[string]any{
		"composerData:a": composerJSON("a", 2, map[string]any{"createdAt": millis(sept10)}),
		// Stored as a BLOB, as Cursor does.
		"composerData:b": []byte(composerJSON("b", 1, map[string]any{
			"createdAt":           millis(sept10.AddDate(0, 0, 11)),
			"workspaceIdentifier": map[string]any{"id": "w", "uri": map[string]any{"$mid": 1, "scheme": "file", "fsPath": "/work/site", "path": "/work/site", "external": "file:///work/site"}},
		})),
		"composerData:c": composerJSON("c", 3, map[string]any{"workspaceIdentifier": map[string]any{"uri": "file:///work/other%20dir"}}),
		"composerData:d": composerJSON("d", 3, map[string]any{"workspaceIdentifier": map[string]any{"uri": map[string]any{"scheme": "vscode-remote", "path": "/srv/x"}}}),
		// Not counted: a draft, a chat without headers, and chats with a
		// transcript on disk (one found by its key, having no composerId).
		"composerData:draft": composerJSON("draft", 2, map[string]any{"isDraft": true}),
		"composerData:empty": composerJSON("empty", 0, nil),
		"composerData:k1":    composerJSON("k1", 4, nil),
		"composerData:k2":    `{"fullConversationHeadersOnly":[{}]}`,
		"composerData:null":  nil,
		"bubbleId:a:1":       `{"type":1,"text":"never read"}`,
	})
	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)

	chats, checked, err := CursorDatabaseReader(home)(context.Background(), map[string]bool{"k1": true, "k2": true})
	if err != nil || !checked {
		t.Fatalf("checked %v, err %v", checked, err)
	}
	got := map[string]CursorDatabaseChat{}
	for _, c := range chats {
		got[c.Folder+"|"+c.CreatedAt.Format(time.RFC3339)] = c
	}
	want := []string{
		"|2026-09-10T09:00:00Z",
		"/work/site|2026-09-21T09:00:00Z",
		"/work/other dir|0001-01-01T00:00:00Z",
		"|0001-01-01T00:00:00Z",
	}
	if len(chats) != len(want) {
		t.Fatalf("got %d chats: %v", len(chats), got)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	assertUnchanged(t, dir, before)
}

func TestCursorDatabaseReaderMissing(t *testing.T) {
	home := t.TempDir()
	chats, checked, err := CursorDatabaseReader(home)(context.Background(), nil)
	if err != nil || !checked || len(chats) != 0 {
		t.Fatalf("chats %d, checked %v, err %v", len(chats), checked, err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading a missing database created something: %v", err)
	}
}

func TestCursorDatabaseReaderNotChecked(t *testing.T) {
	good := map[string]any{"composerData:a": composerJSON("a", 1, nil)}
	for name, setup := range map[string]func(t *testing.T, path string){
		"garbage": func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte("not a database "), 100), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"truncated": func(t *testing.T, path string) {
			writeCursorDB(t, path, false, good)
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(path, data[:len(data)/2], 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"no table": func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT, value BLOB)`); err != nil {
				t.Fatal(err)
			}
			db.Close()
		},
		"not json": func(t *testing.T, path string) {
			writeCursorDB(t, path, false, map[string]any{"composerData:a": composerJSON("a", 1, nil), "composerData:b": "not json"})
		},
		"unknown shape": func(t *testing.T, path string) {
			writeCursorDB(t, path, false, map[string]any{"composerData:a": `{"composerId":"a","isDraft":"no","fullConversationHeadersOnly":[{}]}`})
		},
		"headers not a list": func(t *testing.T, path string) {
			writeCursorDB(t, path, false, map[string]any{"composerData:a": `{"composerId":"a","fullConversationHeadersOnly":{"n":3}}`})
		},
		"directory": func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		// Cursor is not running, so its WAL database has no -wal or -shm;
		// SQLite would create them even for a read-only connection.
		"wal without sidecars": func(t *testing.T, path string) {
			writeCursorDB(t, path, true, good)
		},
		"wal without shm": func(t *testing.T, path string) {
			writeCursorDB(t, path, true, good)
			if err := os.WriteFile(path+"-wal", nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path := CursorStateDatabase(home)
			setup(t, path)
			dir := filepath.Dir(path)
			before := snapshotDir(t, dir)
			chats, checked, err := CursorDatabaseReader(home)(context.Background(), nil)
			if err != nil || checked || len(chats) != 0 {
				t.Fatalf("chats %d, checked %v, err %v", len(chats), checked, err)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestCursorDatabaseReaderLive reads a WAL database while another process,
// standing in for Cursor, holds it open with uncheckpointed writes. It is a
// separate process because SQLite shares one shared-memory mapping between
// connections in a process, so an in-process writer would not show whether
// the reader leaves Cursor's -shm file alone.
func TestCursorDatabaseReaderLive(t *testing.T) {
	home := t.TempDir()
	path := CursorStateDatabase(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCursorWriterProcess$")
	cmd.Env = append(os.Environ(), "BACKFILL_CURSOR_WRITER_DB="+path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer stdin.Close()
	replies := bufio.NewScanner(stdout)
	insert := func(ids ...string) {
		t.Helper()
		for _, id := range ids {
			fmt.Fprintln(stdin, id)
			if !replies.Scan() || replies.Text() != "ok" {
				t.Fatalf("the writer did not insert %s: %q", id, replies.Text())
			}
		}
	}
	insert("a", "b", "k")

	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	if _, ok := before["state.vscdb-wal"]; !ok {
		t.Fatal("the writer has no -wal file")
	}
	chats, checked, err := CursorDatabaseReader(home)(context.Background(), map[string]bool{"k": true})
	if err != nil || !checked || len(chats) != 2 {
		t.Fatalf("chats %d, checked %v, err %v", len(chats), checked, err)
	}
	assertUnchanged(t, dir, before)

	// Cursor keeps writing after the read.
	insert("c")
	if chats, checked, _ := CursorDatabaseReader(home)(context.Background(), map[string]bool{"k": true}); !checked || len(chats) != 3 {
		t.Fatalf("after a write: chats %d, checked %v", len(chats), checked)
	}
}

// TestCursorWriterProcess is the writer process of
// TestCursorDatabaseReaderLive: it inserts one chat per line of stdin, never
// checkpointing, until stdin closes.
func TestCursorWriterProcess(t *testing.T) {
	path := os.Getenv("BACKFILL_CURSOR_WRITER_DB")
	if path == "" {
		t.Skip("run by TestCursorDatabaseReaderLive")
	}
	db := openCursorWriter(t, path, true)
	defer db.Close()
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		id := in.Text()
		insertCursorRows(t, db, map[string]any{"composerData:" + id: composerJSON(id, 1, nil)})
		fmt.Println("ok")
	}
}

// TestCursorDatabaseReaderLocked reads while Cursor holds an exclusive lock:
// the read gives up after the busy timeout and is not checked.
func TestCursorDatabaseReaderLocked(t *testing.T) {
	home := t.TempDir()
	path := CursorStateDatabase(home)
	writeCursorDB(t, path, false, map[string]any{"composerData:a": composerJSON("a", 1, nil)})
	cursor := openCursorWriter(t, path, false)
	defer cursor.Close()
	conn, err := cursor.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	start := time.Now()
	chats, checked, err := CursorDatabaseReader(home)(context.Background(), nil)
	if err != nil || checked || len(chats) != 0 {
		t.Fatalf("chats %d, checked %v, err %v", len(chats), checked, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("a locked database took %v", elapsed)
	}
}

func TestCursorDatabaseReaderCancelled(t *testing.T) {
	home := t.TempDir()
	writeCursorDB(t, CursorStateDatabase(home), false, map[string]any{"composerData:a": composerJSON("a", 1, nil)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := CursorDatabaseReader(home)(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

// TestCursorDatabasePlan runs the real reader under BuildPlan: chats with a
// transcript on disk are not counted, and --since, --until, --project and
// --harness apply where the database has the field.
func TestCursorDatabasePlan(t *testing.T) {
	tr := newTree(t)
	site := tr.repo("home/site")
	other := tr.repo("home/other")
	tr.write(filepath.Join("home", ".cursor", "projects", cursorSlug(site), "agent-transcripts", "k1", "k1.jsonl"), cursorTranscript)
	uri := func(dir string) map[string]any {
		return map[string]any{"uri": map[string]any{"$mid": 1, "scheme": "file", "fsPath": dir, "path": dir}}
	}
	sept := func(day int) int64 { return millis(time.Date(2026, 9, day, 18, 0, 0, 0, time.UTC)) }
	path := CursorStateDatabase(tr.home)
	writeCursorDB(t, path, false, map[string]any{
		"composerData:k1":    composerJSON("k1", 3, map[string]any{"createdAt": sept(20), "workspaceIdentifier": uri(site)}),
		"composerData:early": composerJSON("early", 1, map[string]any{"createdAt": sept(2), "workspaceIdentifier": uri(site)}),
		"composerData:site":  composerJSON("site", 1, map[string]any{"createdAt": sept(21), "workspaceIdentifier": uri(site)}),
		"composerData:other": composerJSON("other", 1, map[string]any{"createdAt": sept(21), "workspaceIdentifier": uri(other)}),
		"composerData:bare":  composerJSON("bare", 1, nil),
		"composerData:draft": composerJSON("draft", 1, map[string]any{"isDraft": true, "createdAt": sept(21)}),
	})
	dir := filepath.Dir(path)
	before := snapshotDir(t, dir)
	env := tr.env()
	env.CursorDatabaseOnly = CursorDatabaseReader(tr.home)

	for _, tc := range []struct {
		name               string
		filters            Filters
		dbOnly, filteredDB int
	}{
		{"no filters", Filters{}, 4, 0},
		{"since", Filters{Since: "2026-09-10"}, 3, 1},
		{"until", Filters{Until: "2026-09-10"}, 2, 2},
		{"project", Filters{Projects: []string{site}}, 3, 1},
		{"project and since", Filters{Projects: []string{site}, Since: "2026-09-10"}, 2, 2},
		{"harness", Filters{Harnesses: []string{"codex"}}, 4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := plan(t, env, nil, config.Config{}, tc.filters)
			if !p.CursorDatabaseChecked || p.CursorDatabaseOnly != tc.dbOnly || p.CursorDatabaseFiltered != tc.filteredDB {
				t.Fatalf("checked %v, only %d, filtered %d", p.CursorDatabaseChecked, p.CursorDatabaseOnly, p.CursorDatabaseFiltered)
			}
			if got := len(p.Candidates) + tc.dbOnly + tc.filteredDB; p.Found() != got {
				t.Fatalf("found %d, want %d", p.Found(), got)
			}
			skipped := p.Skipped()
			if harnessMatches(tc.filters.Harnesses, "cursor") {
				if skipped[SkipCursorDatabaseOnly] != tc.dbOnly {
					t.Fatalf("skipped %v", skipped)
				}
			} else if skipped[SkipCursorDatabaseOnly] != 0 || skipped[SkipFilteredOut] < tc.dbOnly {
				t.Fatalf("skipped %v", skipped)
			}
			var out strings.Builder
			RenderText(&out, p)
			if strings.Contains(out.String(), "were not checked") {
				t.Fatalf("a checked database is reported unchecked:\n%s", out.String())
			}
		})
	}
	assertUnchanged(t, dir, before)

	// A database that can't be read is not checked, and the plan still
	// succeeds.
	if err := os.WriteFile(path, []byte("SQLite format 3\x00 but not really"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := plan(t, env, nil, config.Config{}, Filters{})
	if p.CursorDatabaseChecked || p.CursorDatabaseOnly != 0 {
		t.Fatalf("checked %v, only %d", p.CursorDatabaseChecked, p.CursorDatabaseOnly)
	}
	var out strings.Builder
	RenderText(&out, p)
	if !strings.Contains(out.String(), "were not checked") {
		t.Fatalf("no unchecked line:\n%s", out.String())
	}
}
