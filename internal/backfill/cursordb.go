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
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// CursorDatabaseChat is what the plan needs from one Cursor chat in Cursor's
// database: its ID, to leave out chats with a transcript on disk and to ask
// the archive about it, and enough to apply --since, --until, and --project.
// Nothing else is read, and nothing here is ever printed.
type CursorDatabaseChat struct {
	// ID is composerData.composerId, or the key's suffix without one.
	ID string
	// CreatedAt is composerData.createdAt, zero when absent.
	CreatedAt time.Time
	// Folder is the workspace folder from workspaceIdentifier.uri, "" when
	// the chat names none.
	Folder string
}

// CursorUncheckedReason says why Cursor's database was not checked.
type CursorUncheckedReason string

const (
	// CursorUncheckedLocked: Cursor held a lock past the busy timeout, or a
	// rollback journal shows a write in progress.
	CursorUncheckedLocked CursorUncheckedReason = "locked"
	// CursorUncheckedUnreadable: the file is not a database SQLite can open,
	// or its side files are in a state that can't be read without changing
	// them.
	CursorUncheckedUnreadable CursorUncheckedReason = "unreadable"
	// CursorUncheckedUnknownFormat: the table, a composerData value, or its
	// _v is not a shape this release knows.
	CursorUncheckedUnknownFormat CursorUncheckedReason = "unknown_format"
	// CursorUncheckedChangedDuringRead: Cursor wrote the file while it was
	// read in place with Cursor closed.
	CursorUncheckedChangedDuringRead CursorUncheckedReason = "changed_during_read"
)

// CursorDatabaseResult is one read of Cursor's database. Chats are every
// counted chat, including those with a transcript on disk; the plan leaves
// those out. Reason is set when Checked is false.
type CursorDatabaseResult struct {
	Chats   []CursorDatabaseChat
	Checked bool
	Reason  CursorUncheckedReason
}

// CursorStateDatabase is where Cursor keeps its chats under home.
func CursorStateDatabase(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
}

// cursorBusyTimeout is how long a read waits for Cursor's own lock.
const cursorBusyTimeout = 500 * time.Millisecond

// cursorReadTimeout bounds the whole database read.
const cursorReadTimeout = 30 * time.Second

// maxComposerVersion is the newest composerData _v this release knows. A
// newer one makes the database unknown_format rather than guessing at it.
const maxComposerVersion = 18

// cursorComposerQuery reads the composerData rows. The range, rather than
// LIKE, uses the key's unique index, so the read touches only those rows and
// not every message row, and holds its lock only briefly.
const cursorComposerQuery = `SELECT key, value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;'`

// cursorSideFiles are the files SQLite keeps beside a database.
var cursorSideFiles = []string{"-wal", "-shm", "-journal"}

// cursorAfterRead, when set by a test, runs after an immutable read and
// before its check that the file did not change.
var cursorAfterRead func(path string)

// CursorDatabaseReader returns the Environment.CursorDatabase reader for the
// state.vscdb under home. A missing database is checked with no chats (Cursor
// is not installed). One that can't be read safely is not checked, with a
// reason; that never fails the plan. Only a cancelled context is an error.
func CursorDatabaseReader(home string) func(context.Context) (CursorDatabaseResult, error) {
	path := CursorStateDatabase(home)
	return func(ctx context.Context) (CursorDatabaseResult, error) {
		res := readCursorDatabase(ctx, path)
		if err := ctx.Err(); err != nil {
			return CursorDatabaseResult{}, err
		}
		return res, nil
	}
}

func unchecked(reason CursorUncheckedReason) CursorDatabaseResult {
	return CursorDatabaseResult{Reason: reason}
}

// readCursorDatabase reads path without writing it or creating anything
// beside it. SQLite creates a WAL database's -wal and -shm files when they
// are missing, even for a read-only connection, so there are two ways in:
//
//   - Cursor running: the -wal and -shm files exist. The database is opened
//     in place read-only, with the shared-memory index opened read-only too
//     (readonly_shm), so Cursor's live writes are seen and nothing is
//     created.
//   - Cursor closed: no side file exists, so the database is complete in the
//     one file. It is opened with immutable=1, which opens no side file and
//     takes no lock, and afterwards the file must have the same size,
//     modification time, and inode, with still no side file; otherwise
//     Cursor started and wrote during the read, which immutable=1 could have
//     read torn, and the result is changed_during_read.
//
// Anything else (a rollback journal, or one WAL side file without the other)
// is not checked. If Cursor quits between the side-file check and the open,
// SQLite may create a 0-byte -wal beside the database; that race is accepted.
// A 0-byte -wal is harmless to Cursor, and nothing next to the real database
// is ever deleted.
func readCursorDatabase(ctx context.Context, path string) CursorDatabaseResult {
	before, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return CursorDatabaseResult{Checked: true}
	}
	if err != nil || !before.Mode().IsRegular() {
		return unchecked(CursorUncheckedUnreadable)
	}
	wal, ok := sqliteHeader(path)
	if !ok {
		return unchecked(CursorUncheckedUnreadable)
	}
	sides := existingSideFiles(path)
	var immutable bool
	switch {
	case len(sides) == 0:
		immutable = true
	case sides["-journal"]:
		return unchecked(CursorUncheckedLocked)
	case wal && sides["-wal"] && sides["-shm"]:
	default:
		return unchecked(CursorUncheckedUnreadable)
	}

	query := url.Values{}
	query.Set("mode", "ro")
	if immutable {
		query.Set("immutable", "1")
	} else {
		query.Set("readonly_shm", "1")
	}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", cursorBusyTimeout.Milliseconds()))
	query.Add("_pragma", "query_only(1)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()

	ctx, cancel := context.WithTimeout(ctx, cursorReadTimeout)
	defer cancel()
	res := queryCursorDatabase(ctx, dsn)
	if immutable {
		if cursorAfterRead != nil {
			cursorAfterRead(path)
		}
		after, err := os.Stat(path)
		if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) ||
			!os.SameFile(before, after) || len(existingSideFiles(path)) != 0 {
			return unchecked(CursorUncheckedChangedDuringRead)
		}
	}
	return res
}

func queryCursorDatabase(ctx context.Context, dsn string) CursorDatabaseResult {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return unchecked(uncheckedReason(err))
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, cursorComposerQuery)
	if err != nil {
		return unchecked(uncheckedReason(err))
	}
	defer rows.Close()

	var chats []CursorDatabaseChat
	subagents := map[string]bool{}
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return unchecked(uncheckedReason(err))
		}
		if value == nil {
			continue
		}
		d, ok := decodeComposerData(key, value)
		if !ok {
			return unchecked(CursorUncheckedUnknownFormat)
		}
		for _, id := range d.subagents {
			subagents[id] = true
		}
		if d.counted {
			chats = append(chats, d.chat)
		}
	}
	if err := rows.Err(); err != nil {
		return unchecked(uncheckedReason(err))
	}
	// A subagent's composer is part of its parent chat, not a chat of its
	// own.
	kept := chats[:0]
	for _, c := range chats {
		if !subagents[c.ID] {
			kept = append(kept, c)
		}
	}
	return CursorDatabaseResult{Chats: kept, Checked: true}
}

// uncheckedReason classifies a SQLite error.
func uncheckedReason(err error) CursorUncheckedReason {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return CursorUncheckedLocked
		}
	}
	if msg := err.Error(); strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
		return CursorUncheckedUnknownFormat
	}
	return CursorUncheckedUnreadable
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

// existingSideFiles reports which of path's side files exist.
func existingSideFiles(path string) map[string]bool {
	out := map[string]bool{}
	for _, suffix := range cursorSideFiles {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			out[suffix] = true
		}
	}
	return out
}

// composer is what one composerData value contributes.
type composer struct {
	chat CursorDatabaseChat
	// counted is whether it is a chat: messages, and not a draft.
	counted bool
	// subagents are the composer IDs of its subagents.
	subagents []string
}

// decodeComposerData decodes one composerData value; ok is false when it is
// not a shape this release knows. Only the fields that decide the count are
// strict: _v, isDraft, and the message lists. The rest are read leniently and
// ignored when they are some other shape.
func decodeComposerData(key string, value []byte) (composer, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil || fields == nil {
		return composer{}, false
	}
	present := func(name string) (json.RawMessage, bool) {
		raw, ok := fields[name]
		if !ok || string(raw) == "null" {
			return nil, false
		}
		return raw, true
	}
	if raw, ok := present("_v"); ok {
		var v int
		if json.Unmarshal(raw, &v) != nil || v < 1 || v > maxComposerVersion {
			return composer{}, false
		}
	}
	var isDraft bool
	if raw, ok := present("isDraft"); ok && json.Unmarshal(raw, &isDraft) != nil {
		return composer{}, false
	}
	// fullConversationHeadersOnly lists the messages; older chats keep them
	// inline in conversation instead.
	messages := 0
	for _, name := range []string{"fullConversationHeadersOnly", "conversation"} {
		if raw, ok := present(name); ok {
			var list []json.RawMessage
			if json.Unmarshal(raw, &list) != nil {
				return composer{}, false
			}
			messages += len(list)
		}
	}

	var c composer
	if raw, ok := present("composerId"); ok {
		json.Unmarshal(raw, &c.chat.ID)
	}
	if c.chat.ID == "" {
		c.chat.ID = strings.TrimPrefix(key, "composerData:")
	}
	if raw, ok := present("subagentComposerIds"); ok {
		var ids []json.RawMessage
		if json.Unmarshal(raw, &ids) == nil {
			for _, rawID := range ids {
				var id string
				if json.Unmarshal(rawID, &id) == nil && id != "" {
					c.subagents = append(c.subagents, id)
				}
			}
		}
	}
	c.counted = !isDraft && messages > 0
	if !c.counted {
		return c, true
	}
	if raw, ok := present("createdAt"); ok {
		var ms float64
		if json.Unmarshal(raw, &ms) == nil && ms > 0 {
			c.chat.CreatedAt = time.UnixMilli(int64(ms)).UTC()
		}
	}
	if raw, ok := present("workspaceIdentifier"); ok {
		var ws map[string]json.RawMessage
		if json.Unmarshal(raw, &ws) == nil {
			c.chat.Folder = composerWorkspaceFolder(ws["uri"])
		}
	}
	return c, true
}

// composerWorkspaceFolder turns workspaceIdentifier.uri into a local folder:
// either a file:// URI string or a VS Code URI object (scheme, fsPath, path,
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
// gives each exactly one outcome, as file sessions get. The database is not
// opened when --harness leaves Cursor out. A chat with a transcript on disk
// is left out (the file wins). The archive's own reasons (already_archived,
// removed_*) come first, then the filters: --since and --until use
// createdAt, and --project the workspace folder's project; as for a file
// session, a chat without the field does not match. The rest are
// cursor_database_only.
func countCursorDatabase(ctx context.Context, env Environment, state ArchiveState, r *resolver, projectFilter []string, since, until time.Time, plan *Plan) error {
	if env.CursorDatabase == nil || !harnessMatches(plan.Filters.Harnesses, "cursor") {
		return nil
	}
	res, err := env.CursorDatabase(ctx)
	if err != nil {
		return fmt.Errorf("count Cursor database chats: %w", err)
	}
	plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = res.Checked, res.Reason
	if !res.Checked {
		return nil
	}
	fileChats := map[string]bool{}
	for _, c := range plan.Candidates {
		if c.Harness == "cursor" {
			fileChats[c.NativeSessionID] = true
		}
	}
	dated := !since.IsZero() || !until.IsZero()
	seen := map[string]bool{}
	for _, chat := range res.Chats {
		if fileChats[chat.ID] || seen[chat.ID] {
			continue
		}
		seen[chat.ID] = true
		if strings.TrimSpace(chat.ID) != "" {
			reason, err := state.Classify("cursor", chat.ID)
			if err != nil {
				return fmt.Errorf("check the archive: %w", err)
			}
			if plan.Filters.IncludeRemoved && (reason == SkipRemovedByUndo || reason == SkipRemovedByRetention) {
				reason = ""
			}
			if reason != "" {
				if plan.CursorDatabaseSkipped == nil {
					plan.CursorDatabaseSkipped = map[SkipReason]int{}
				}
				plan.CursorDatabaseSkipped[reason]++
				continue
			}
		}
		filtered := dated && !inRange(chat.CreatedAt, since, until)
		if !filtered && len(projectFilter) > 0 {
			root := ""
			if chat.Folder != "" {
				root = r.resolve(chat.Folder).root
			}
			filtered = !projectMatches(env, projectFilter, root)
		}
		if filtered {
			plan.CursorDatabaseFiltered++
		} else {
			plan.CursorDatabaseOnly++
		}
	}
	return nil
}
