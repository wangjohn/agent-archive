package backfill

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/cursorstore"
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
type CursorUncheckedReason = cursorstore.Reason

const (
	// CursorUncheckedLocked: a rollback journal shows an unfinished write
	// (which may be a hot journal only Cursor can roll back), or Cursor held
	// a lock past the busy timeout.
	CursorUncheckedLocked = cursorstore.Locked
	// CursorUncheckedUnreadable: the file is not a database SQLite can open,
	// or its side files are in a state that can't be read without changing
	// them.
	CursorUncheckedUnreadable = cursorstore.Unreadable
	// CursorUncheckedUnknownFormat: the table, a composerData value, or its
	// _v is not a shape this release knows.
	CursorUncheckedUnknownFormat = cursorstore.UnknownFormat
	// CursorUncheckedChangedDuringRead: Cursor wrote the file while it was
	// read in place with Cursor closed.
	CursorUncheckedChangedDuringRead = cursorstore.ChangedDuringRead
	// CursorUncheckedTranscriptsUnreadable: part of Cursor's transcript store
	// could not be listed, so a chat with a transcript can't be told apart
	// from one stored only in the database. The database is not opened.
	CursorUncheckedTranscriptsUnreadable CursorUncheckedReason = "transcripts_unreadable"
)

// CursorDatabaseResult is one read of Cursor's database. Chats are every
// counted chat, including those with a transcript on disk; the plan leaves
// those out. Reason is set when Checked is false.
type CursorDatabaseResult struct {
	Chats   []CursorDatabaseChat
	Checked bool
	Reason  CursorUncheckedReason
	// NewerFormat counts the composerData rows with a _v newer than this
	// release knows, read anyway.
	NewerFormat int
}

// CursorStateDatabase is where Cursor keeps its chats under home.
func CursorStateDatabase(home string) string {
	return cursorstore.StateDatabase(home)
}

// maxComposerVersion is the newest composerData _v this release knows. Newer
// rows are still counted when the fields the count needs decode, and the plan
// reports how many there were.
const maxComposerVersion = 18

// cursorComposerQuery reads the composerData rows. The range, rather than
// LIKE, uses the key's unique index, so the read touches only those rows and
// not every message row, and holds its lock only briefly.
const cursorComposerQuery = `SELECT key, value FROM cursorDiskKV WHERE key >= 'composerData:' AND key < 'composerData;'`

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

// readCursorDatabase reads path through cursorstore.Read, which neither
// writes it nor creates anything beside it (see cursorstore's resolve for
// how it opens a database with Cursor running and closed).
func readCursorDatabase(ctx context.Context, path string) CursorDatabaseResult {
	var res CursorDatabaseResult
	err := cursorstore.Read(ctx, path, cursorstore.Options{AfterImmutableRead: cursorAfterRead}, func(ctx context.Context, db *sql.DB) error {
		var err error
		res, err = queryCursorDatabase(ctx, db)
		return err
	})
	switch {
	case errors.Is(err, cursorstore.ErrNoDatabase):
		return CursorDatabaseResult{Checked: true}
	case err != nil:
		return unchecked(cursorstore.ReasonOf(err))
	}
	return res
}

func queryCursorDatabase(ctx context.Context, db *sql.DB) (CursorDatabaseResult, error) {
	rows, err := db.QueryContext(ctx, cursorComposerQuery)
	if err != nil {
		return CursorDatabaseResult{}, err
	}
	defer rows.Close()

	var chats []CursorDatabaseChat
	newer := 0
	subagents := map[string]bool{}
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return CursorDatabaseResult{}, err
		}
		if value == nil {
			continue
		}
		d, ok := decodeComposerData(key, value)
		if !ok {
			return CursorDatabaseResult{}, cursorstore.NotChecked(cursorstore.UnknownFormat)
		}
		if d.newer {
			newer++
		}
		for _, id := range d.subagents {
			subagents[id] = true
		}
		if d.counted {
			chats = append(chats, d.chat)
		}
	}
	if err := rows.Err(); err != nil {
		return CursorDatabaseResult{}, err
	}
	// A subagent's composer is part of its parent chat, not a chat of its
	// own.
	kept := chats[:0]
	for _, c := range chats {
		if !subagents[c.ID] {
			kept = append(kept, c)
		}
	}
	return CursorDatabaseResult{Chats: kept, Checked: true, NewerFormat: newer}, nil
}

// composer is what one composerData value contributes.
type composer struct {
	chat CursorDatabaseChat
	// counted is whether it is a chat: messages, and not a draft.
	counted bool
	// subagents are the composer IDs of its subagents.
	subagents []string
	// newer is set when its _v is newer than this release knows.
	newer bool
}

// decodeComposerData decodes one composerData value; ok is false when it is
// not a shape this release knows. Only the fields that decide the count are
// strict: isDraft and the message lists, and _v must be a positive integer.
// A _v newer than maxComposerVersion is still read, and marked newer, when
// those fields decode. The rest are read leniently and ignored when they are
// some other shape.
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
	var c composer
	if raw, ok := present("_v"); ok {
		var v int
		if json.Unmarshal(raw, &v) != nil || v < 1 {
			return composer{}, false
		}
		c.newer = v > maxComposerVersion
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
	if plan.cursorIncomplete {
		plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = false, CursorUncheckedTranscriptsUnreadable
		return nil
	}
	res, err := env.CursorDatabase(ctx)
	if err != nil {
		return fmt.Errorf("count Cursor database chats: %w", err)
	}
	plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = res.Checked, res.Reason
	plan.CursorDatabaseNewerFormat = res.NewerFormat
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
	skip := func(reason SkipReason) {
		if plan.CursorDatabaseSkipped == nil {
			plan.CursorDatabaseSkipped = map[SkipReason]int{}
		}
		plan.CursorDatabaseSkipped[reason]++
	}
	seen := map[string]bool{}
	for _, chat := range res.Chats {
		if fileChats[chat.ID] {
			continue
		}
		// Two rows with one composerId are one chat found twice.
		if seen[chat.ID] {
			skip(SkipDuplicateSession)
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
				skip(reason)
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
