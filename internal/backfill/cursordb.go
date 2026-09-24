package backfill

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	// modernc.org/sqlite is a pure-Go SQLite, so builds and tests need no
	// cgo (spec, "Phase 2: Cursor database chats", decision 4).
	_ "modernc.org/sqlite"
)

// CursorDatabaseChat is what the plan needs from one chat found only in
// Cursor's database: enough to apply --since, --until, and --project. Nothing
// else is read, and nothing here is ever printed.
type CursorDatabaseChat struct {
	// CreatedAt is composerData.createdAt, zero when absent.
	CreatedAt time.Time
	// Folder is the workspace folder from workspaceIdentifier.uri, "" when
	// the chat names none.
	Folder string
}

// CursorStateDatabase is where Cursor keeps its chats under home.
func CursorStateDatabase(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
}

// cursorBusyTimeout is how long a read waits for Cursor's own lock.
const cursorBusyTimeout = 500 * time.Millisecond

// cursorReadTimeout bounds the whole database read.
const cursorReadTimeout = 30 * time.Second

// CursorDatabaseReader returns the Environment.CursorDatabaseOnly reader for
// the state.vscdb under home. A missing database is checked with no chats
// (Cursor is not installed). A database that can't be read read-only without
// changing anything beside it, is locked, or has a table or JSON shape this
// release does not know is reported as not checked; that never fails the
// plan. Only a cancelled context is an error.
func CursorDatabaseReader(home string) func(context.Context, map[string]bool) ([]CursorDatabaseChat, bool, error) {
	path := CursorStateDatabase(home)
	return func(ctx context.Context, fileChats map[string]bool) ([]CursorDatabaseChat, bool, error) {
		chats, checked := readCursorDatabase(ctx, path, fileChats)
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		return chats, checked, nil
	}
}

// readCursorDatabase opens path strictly read-only. It never writes the
// database and never creates a file beside it: SQLite creates the -wal and
// -shm files of a WAL database when they are missing, even for a read-only
// connection, so a WAL database is read only while Cursor has them open, and
// is otherwise not checked. immutable=1 is never used, because it can read
// torn pages while Cursor writes.
func readCursorDatabase(ctx context.Context, path string, fileChats map[string]bool) ([]CursorDatabaseChat, bool) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	wal, ok := sqliteHeader(path)
	if !ok {
		return nil, false
	}
	if wal && (!cursorSidecarExists(path+"-wal") || !cursorSidecarExists(path+"-shm")) {
		return nil, false
	}

	query := url.Values{}
	query.Set("mode", "ro")
	// The shared-memory index is opened read-only and never created.
	query.Set("readonly_shm", "1")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", cursorBusyTimeout.Milliseconds()))
	query.Add("_pragma", "query_only(1)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()

	ctx, cancel := context.WithTimeout(ctx, cursorReadTimeout)
	defer cancel()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()

	var chats []CursorDatabaseChat
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, false
		}
		if value == nil {
			continue
		}
		chat, id, counted, ok := decodeComposerData(key, value)
		if !ok {
			return nil, false
		}
		if counted && !fileChats[id] {
			chats = append(chats, chat)
		}
	}
	if rows.Err() != nil {
		return nil, false
	}
	return chats, true
}

// sqliteHeader reads the 100-byte database header: ok is false for a file
// that is not a SQLite database, and wal is whether it is in WAL mode (file
// format read or write version 2).
func sqliteHeader(path string) (wal, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(f, header); err != nil {
		return false, false
	}
	if !bytes.Equal(header[:16], []byte("SQLite format 3\x00")) {
		return false, false
	}
	return header[18] == 2 || header[19] == 2, true
}

func cursorSidecarExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// composerData holds the fields of a composerData:<id> value the count reads.
type composerData struct {
	ComposerID string `json:"composerId"`
	IsDraft    bool   `json:"isDraft"`
	// Headers is fullConversationHeadersOnly; only its length is used.
	Headers             []json.RawMessage `json:"fullConversationHeadersOnly"`
	CreatedAt           json.RawMessage   `json:"createdAt"`
	WorkspaceIdentifier *struct {
		URI json.RawMessage `json:"uri"`
	} `json:"workspaceIdentifier"`
}

// decodeComposerData decodes one composerData value. counted is whether it is
// a chat the count includes (it has headers and is not a draft); ok is false
// when the value is not the shape this release knows.
func decodeComposerData(key string, value []byte) (chat CursorDatabaseChat, id string, counted, ok bool) {
	var d composerData
	if err := json.Unmarshal(value, &d); err != nil {
		return CursorDatabaseChat{}, "", false, false
	}
	id = d.ComposerID
	if id == "" {
		id = strings.TrimPrefix(key, "composerData:")
	}
	if d.IsDraft || len(d.Headers) == 0 {
		return CursorDatabaseChat{}, id, false, true
	}
	var ms float64
	if len(d.CreatedAt) > 0 && json.Unmarshal(d.CreatedAt, &ms) == nil && ms > 0 {
		chat.CreatedAt = time.UnixMilli(int64(ms)).UTC()
	}
	if d.WorkspaceIdentifier != nil {
		chat.Folder = composerWorkspaceFolder(d.WorkspaceIdentifier.URI)
	}
	return chat, id, true, true
}

// composerWorkspaceFolder turns workspaceIdentifier.uri into a local folder: either a
// file:// URI string or a VS Code URI object (scheme, fsPath, path,
// external). Anything else, or a folder that is not local, is "".
func composerWorkspaceFolder(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	fromURI := func(s string) string {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || !filepath.IsAbs(u.Path) {
			return ""
		}
		return filepath.Clean(u.Path)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return fromURI(s)
	}
	var obj struct {
		Scheme    string `json:"scheme"`
		Authority string `json:"authority"`
		FSPath    string `json:"fsPath"`
		Path      string `json:"path"`
		External  string `json:"external"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if obj.Scheme != "" && obj.Scheme != "file" {
		return ""
	}
	if obj.Authority != "" {
		return ""
	}
	for _, p := range []string{obj.FSPath, obj.Path} {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
	}
	if obj.External != "" {
		return fromURI(obj.External)
	}
	return ""
}

// countCursorDatabase reads the chats found only in Cursor's database and
// splits them into those the filters keep and those they leave out. --harness
// is applied when the plan is shown (Plan.Skipped). --since and --until use
// createdAt, and --project the workspace folder's project; a chat without the
// needed field is kept, since phase 1 does not import it anyway.
func countCursorDatabase(ctx context.Context, env Environment, r *resolver, projectFilter []string, since, until time.Time, plan *Plan) error {
	if env.CursorDatabaseOnly == nil {
		return nil
	}
	fileChats := map[string]bool{}
	for _, c := range plan.Candidates {
		if c.Harness == "cursor" {
			fileChats[c.NativeSessionID] = true
		}
	}
	chats, checked, err := env.CursorDatabaseOnly(ctx, fileChats)
	if err != nil {
		return fmt.Errorf("count Cursor database chats: %w", err)
	}
	plan.CursorDatabaseChecked = checked
	if !checked {
		return nil
	}
	dated := !since.IsZero() || !until.IsZero()
	for _, chat := range chats {
		filtered := dated && !chat.CreatedAt.IsZero() && !inRange(chat.CreatedAt, since, until)
		if !filtered && len(projectFilter) > 0 && chat.Folder != "" {
			if root := r.resolve(chat.Folder).root; root != "" {
				filtered = !projectMatches(env, projectFilter, root)
			}
		}
		if filtered {
			plan.CursorDatabaseFiltered++
		} else {
			plan.CursorDatabaseOnly++
		}
	}
	return nil
}
