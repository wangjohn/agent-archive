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
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// CursorDatabaseChat is what the plan lists of one Cursor chat in Cursor's
// database: its ID, to leave out chats with a transcript on disk and to ask
// the archive about it, its start, and where its project may be named. The
// plan reads the chat whole (CursorDatabaseResult.ReadChat) only when it may
// be imported. Nothing here is ever printed.
type CursorDatabaseChat struct {
	// ID is composerData.composerId, or the key's suffix without one.
	ID string
	// KeyID is the composerData:<id> key's suffix, which the chat is read
	// by. A chat whose composerId differs from it is identity_mismatch.
	KeyID string
	// CreatedAt is composerData.createdAt, zero when absent.
	CreatedAt time.Time
	// Folder is the workspace folder from workspaceIdentifier.uri, "" when
	// the chat names none.
	Folder string
	// WorkspaceID is workspaceIdentifier.id, the workspaceStorage folder of
	// the chat's workspace, "" when absent.
	WorkspaceID string
	// Malformed is set when composerId or workspaceIdentifier.id is present
	// but not a string. The chat is not read and counts as unsafe_format,
	// rather than being imported under an ID or workspace it does not have.
	Malformed bool
}

// CursorUncheckedReason says why Cursor's database was not checked.
type CursorUncheckedReason = cursorstore.Reason

const (
	// CursorUncheckedLocked means a rollback journal shows an unfinished
	// write (which may be a hot journal only Cursor can roll back) or that
	// Cursor held a lock past the busy timeout.
	CursorUncheckedLocked = cursorstore.Locked
	// CursorUncheckedUnreadable means the file is not a database SQLite can
	// open, or its side files are in a state that can't be read without
	// changing them.
	CursorUncheckedUnreadable = cursorstore.Unreadable
	// CursorUncheckedUnknownFormat means the table, a composerData value, or
	// its _v is not a shape this release knows.
	CursorUncheckedUnknownFormat = cursorstore.UnknownFormat
	// CursorUncheckedChangedDuringRead means Cursor wrote the file while it
	// was read in place with Cursor closed.
	CursorUncheckedChangedDuringRead = cursorstore.ChangedDuringRead
	// CursorUncheckedTranscriptsUnreadable means part of Cursor's transcript
	// store could not be listed, so a chat with a transcript can't be told
	// apart from one stored only in the database. The database is not opened.
	CursorUncheckedTranscriptsUnreadable CursorUncheckedReason = "transcripts_unreadable"
)

// CursorDatabaseResult is one read of Cursor's database. Chats are every
// chat with messages that is not a draft and not a subagent, including those
// with a transcript on disk; the plan leaves those out. Reason is set when
// Checked is false.
type CursorDatabaseResult struct {
	Chats   []CursorDatabaseChat
	Checked bool
	Reason  CursorUncheckedReason
	// NewerFormat counts the composerData rows with a _v newer than this
	// release knows, read anyway.
	NewerFormat int
	// Subagents maps a chat's ID to the IDs of its subagent chats
	// (subagentComposerIds) that have messages and are not drafts.
	Subagents map[string][]string
	// ReadChat reads one listed chat whole, by its KeyID, as the collector
	// will: through one cursorstore.Reader for the whole plan, so at most
	// one snapshot of the database. It is not safe for concurrent use. A
	// chat Cursor deleted since the listing wraps fs.ErrNotExist. Set when
	// Checked.
	ReadChat func(ctx context.Context, id string) (cursorstore.Composer, error)
	// Close removes the snapshot ReadChat took, if any. Set when Checked.
	Close func() error
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

// CursorDatabaseReader returns the Environment.CursorDatabase reader for the
// state.vscdb under home. A missing database is checked with no chats (Cursor
// is not installed). One that can't be read safely is not checked, with a
// reason; that never fails the plan. Only a cancelled context is an error.
// The chats are listed in place; each one the plan reads whole goes through
// one cursorstore.Reader, whose snapshot, when Cursor is running, lives in
// the per-user temporary directory until Close. Nothing is written beside
// the database.
func CursorDatabaseReader(home string) func(context.Context) (CursorDatabaseResult, error) {
	path := CursorStateDatabase(home)
	return func(ctx context.Context) (CursorDatabaseResult, error) {
		res := readCursorDatabase(ctx, path, cursorstore.Options{})
		if err := ctx.Err(); err != nil {
			return CursorDatabaseResult{}, err
		}
		if res.Checked {
			// A copy an earlier plan left when it was killed goes before this
			// plan can take another.
			cursorstore.RemoveStaleSnapshots()
			reader := cursorstore.NewReader(path)
			res.ReadChat = func(ctx context.Context, id string) (cursorstore.Composer, error) {
				c, _, err := reader.ReadComposer(ctx, id)
				return c, err
			}
			res.Close = reader.Close
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
func readCursorDatabase(ctx context.Context, path string, opts cursorstore.Options) CursorDatabaseResult {
	var res CursorDatabaseResult
	err := cursorstore.Read(ctx, path, opts, func(ctx context.Context, db *sql.DB) error {
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
	defer func() { _ = rows.Close() }()

	var chats []CursorDatabaseChat
	newer := 0
	subagents := map[string]bool{}
	parents := map[string][]string{}
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
		if len(d.subagents) > 0 {
			parents[d.chat.ID] = append(parents[d.chat.ID], d.subagents...)
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
	counted := map[string]bool{}
	kept := chats[:0]
	for _, c := range chats {
		counted[c.ID] = true
		if !subagents[c.ID] {
			kept = append(kept, c)
		}
	}
	res := CursorDatabaseResult{Chats: kept, Checked: true, NewerFormat: newer}
	for parent, ids := range parents {
		for _, id := range ids {
			if counted[id] {
				if res.Subagents == nil {
					res.Subagents = map[string][]string{}
				}
				res.Subagents[parent] = append(res.Subagents[parent], id)
			}
		}
	}
	return res, nil
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
// those fields decode. A composerId or workspaceIdentifier.id of another
// shape marks the chat Malformed (that chat's unsafe_format). The rest are
// read leniently and ignored when they are some other shape.
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
	newer := false
	if raw, ok := present("_v"); ok {
		var v int
		if json.Unmarshal(raw, &v) != nil || v < 1 {
			return composer{}, false
		}
		newer = v > maxComposerVersion
	}
	var isDraft bool
	if raw, ok := present("isDraft"); ok && json.Unmarshal(raw, &isDraft) != nil {
		return composer{}, false
	}
	// fullConversationHeadersOnly lists the messages; older chats keep them
	// inline in conversation instead. Messages are counted as cursorstore
	// reads a chat (decodeHeaders): from the headers when the chat has them,
	// even an empty list, and from conversation only when it has none, so a
	// chat is planned only when the collector would find messages in it.
	// Each header's own shape is not checked here: a header the reader can't
	// use makes that one chat unsafe_format when it is read, whereas a
	// listing error leaves the whole database unchecked.
	messages := 0
	for _, name := range []string{"fullConversationHeadersOnly", "conversation"} {
		if raw, ok := present(name); ok {
			var list []json.RawMessage
			if json.Unmarshal(raw, &list) != nil {
				return composer{}, false
			}
			messages = len(list)
			break
		}
	}

	keyID := strings.TrimPrefix(key, "composerData:")
	var id string
	malformed := false
	if raw, ok := present("composerId"); ok {
		if json.Unmarshal(raw, &id) != nil {
			malformed = true
		}
	}
	if id == "" {
		id = keyID
	}
	var subagents []string
	if raw, ok := present("subagentComposerIds"); ok {
		var ids []json.RawMessage
		if json.Unmarshal(raw, &ids) == nil {
			for _, rawID := range ids {
				var subagentID string
				if json.Unmarshal(rawID, &subagentID) == nil && subagentID != "" {
					subagents = append(subagents, subagentID)
				}
			}
		}
	}
	counted := !isDraft && messages > 0

	// The rest is read only for a chat that is counted.
	var createdAt time.Time
	var folder, workspaceID string
	if counted {
		if raw, ok := present("createdAt"); ok {
			var ms float64
			if json.Unmarshal(raw, &ms) == nil && ms > 0 {
				createdAt = time.UnixMilli(int64(ms)).UTC()
			}
		}
		if raw, ok := present("workspaceIdentifier"); ok {
			var ws map[string]json.RawMessage
			if json.Unmarshal(raw, &ws) == nil {
				folder = composerWorkspaceFolder(ws["uri"])
				if rawID, ok := ws["id"]; ok {
					if json.Unmarshal(rawID, &workspaceID) != nil {
						malformed = true
					}
				}
			}
		}
	}
	return composer{
		chat: CursorDatabaseChat{
			ID:          id,
			KeyID:       keyID,
			CreatedAt:   createdAt,
			Folder:      folder,
			WorkspaceID: workspaceID,
			Malformed:   malformed,
		},
		counted:   counted,
		subagents: subagents,
		newer:     newer,
	}, true
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

// planCursorDatabase adds the chats found only in Cursor's database to the
// plan as candidates, each with exactly one outcome, as file sessions get.
// The database is not opened when --harness leaves Cursor out, or when
// Cursor's transcript store could not be fully listed. A chat with a
// transcript on disk is left out (the file wins); so are drafts and
// subagent chats, which the listing already leaves out.
//
// The archive's own reasons (already_archived, removed_*) and --since and
// --until (on createdAt) are decided from the listing. Every chat that may
// still be imported is then read whole and filtered with the collector's own
// code, through one Reader, so the plan's empty, unsafe_format, and
// too_large are what the collector would decide. Its project is the folder
// workspaceIdentifier.uri names, then the folder of the workspace
// workspaceIdentifier.id names (workspaceStorage/<id>/workspace.json), then
// the one folder its messages' workspaceUris agree on; the folder then goes
// through the same project rules as any working directory, and a chat with
// none is project_unknown. If a chat can't be read safely after all (Cursor
// held a lock, the copy failed), the database counts as not checked and none
// of its chats is included, as when it can't be listed.
func planCursorDatabase(ctx context.Context, env Environment, state ArchiveState, r *resolver, projectFilter []string, since, until time.Time, workers int, plan *Plan) (err error) {
	if env.CursorDatabase == nil || !harnessMatches(plan.Filters.Harnesses, "cursor") {
		return nil
	}
	if plan.cursorIncomplete {
		plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = false, CursorUncheckedTranscriptsUnreadable
		return nil
	}
	res, err := env.CursorDatabase(ctx)
	if err != nil {
		return fmt.Errorf("read Cursor's database: %w", err)
	}
	if res.Close != nil {
		// The plan's copy of the database holds every chat; one that can't
		// be removed is reported, not left silently in the temporary folder
		// (the next sweep removes it once it is stale).
		defer func() {
			if closeErr := res.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}()
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
	var items, toRead []*work
	// Two rows with one composerId are one chat found twice. The row kept is
	// one whose key is its ID, which the chat can be read back by, so a
	// stray row naming another chat's ID never displaces the real one.
	kept := map[string]int{}
	for i, chat := range res.Chats {
		if k, ok := kept[chat.ID]; !ok || (res.Chats[k].KeyID != chat.ID && chat.KeyID == chat.ID) {
			kept[chat.ID] = i
		}
	}
	for i, chat := range res.Chats {
		if fileChats[chat.ID] {
			continue
		}
		w := &work{
			t:    &transcript{harness: harnessCursor, nativeID: chat.ID},
			chat: chat,
			c: Candidate{
				Harness: string(harnessCursor), NativeSessionID: chat.ID,
				SourceKind: archive.SourceKindCursorSQLite, SourceKey: chat.ID,
				StartedAt: chat.CreatedAt, StartedAtSource: archive.StartedAtSourceCursorComposer,
			},
		}
		items = append(items, w)
		if kept[chat.ID] != i {
			w.duplicate = true
			continue
		}
		// The collector reads the chat by its ID; a row whose composerId is
		// not its key's can't be read back under the ID it would register.
		w.t.identityMismatch = chat.ID != chat.KeyID
		if strings.TrimSpace(chat.ID) != "" {
			reason, err := state.Classify("cursor", chat.ID)
			if err != nil {
				return fmt.Errorf("check the archive: %w", err)
			}
			if plan.Filters.IncludeRemoved && (reason == SkipRemovedByUndo || reason == SkipRemovedByRetention) {
				reason = ""
			}
			w.state = reason
		}
		w.filtered = dated && !inRange(chat.CreatedAt, since, until)
		// A chat whose ID or workspace field is of another shape is that
		// chat's unsafe_format, reported, not read.
		w.unsafe = chat.Malformed
		if w.state == "" && !w.filtered && !w.t.identityMismatch && !w.unsafe {
			toRead = append(toRead, w)
		}
	}

	if res.ReadChat == nil {
		plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = false, CursorUncheckedUnreadable
		return nil
	}
	// Reads go one at a time through the plan's one Reader; filtering, the
	// costly part, runs on the workers.
	var mu sync.Mutex
	var readErr error
	if err := forEach(ctx, workers, toRead, func(w *work) {
		mu.Lock()
		c, err := res.ReadChat(ctx, w.chat.KeyID)
		// A value of this chat's that does not decode is the chat's
		// problem, unsafe_format; only a failure of the database itself
		// (a lock, a failed copy, a changed file) leaves it unchecked.
		chatOnly := err != nil && (isNotExist(err) || cursorstore.ReasonOf(err) == cursorstore.UnknownFormat)
		if err != nil && !chatOnly && readErr == nil {
			readErr = err
		}
		mu.Unlock()
		if err != nil {
			// A chat Cursor deleted since the listing is not counted at all.
			w.vanished = isNotExist(err)
			w.unsafe = !w.vanished
			return
		}
		w.c.Bytes = collector.CursorChatSize(c)
		w.messageFolders = messageWorkspaceFolders(c)
		filtered, err := collector.FilterCursorChat(c)
		switch {
		case errors.Is(err, archive.ErrRecordTooLarge):
			w.tooLarge = true
		case err != nil:
			w.unsafe = true
		default:
			w.empty = !carriesConversation(filtered)
		}
	}); err != nil {
		return err
	}
	if readErr != nil {
		plan.CursorDatabaseChecked, plan.CursorDatabaseUnchecked = false, cursorstore.ReasonOf(readErr)
		return nil
	}

	for _, w := range items {
		if w.vanished {
			continue
		}
		if !w.duplicate {
			if folder := cursorChatFolder(env, w.chat, w.messageFolders); folder != "" {
				w.res = r.resolve(folder)
			} else {
				w.res = resolution{skip: SkipProjectUnknown}
			}
			if !projectMatches(env, projectFilter, w.res.root) {
				w.filtered = true
			}
		}
		w.c.ProjectRoot, w.c.ProjectKind, w.c.ProjectIncluded = w.res.root, w.res.kind, w.res.included
		if w.res.root != "" {
			w.c.ProjectExists = env.exists(w.res.root)
		}
		w.c.Skip = w.reason(plan.GeneratedAt)
		plan.Candidates = append(plan.Candidates, w.c)
	}

	// Subagent chats are not imported yet: count those of every Cursor chat
	// this plan imports, file or database, so the plan can say so.
	for _, c := range plan.Candidates {
		if c.Harness == "cursor" && c.Skip == "" {
			plan.CursorSubagentsNotImported += len(res.Subagents[c.NativeSessionID])
		}
	}
	return nil
}

// cursorChatFolder is the folder a database chat's project is resolved
// from: workspaceIdentifier.uri, then the workspace.json of the workspace
// workspaceIdentifier.id names, then the one folder the chat's messages name
// in workspaceUris. "" when none is known.
func cursorChatFolder(env Environment, chat CursorDatabaseChat, messageFolders []string) string {
	if chat.Folder != "" {
		return chat.Folder
	}
	if id := chat.WorkspaceID; id != "" && id == filepath.Base(id) && id != "." && id != ".." {
		data, err := env.readFile(filepath.Join(cursorWorkspaceStorage(env), id, "workspace.json"))
		if err == nil {
			if folder := workspaceJSONFolder(data); folder != "" {
				return folder
			}
		}
	}
	if len(messageFolders) == 1 {
		return messageFolders[0]
	}
	return ""
}

// messageWorkspaceFolders are the distinct local folders a chat's messages
// name in workspaceUris, in order. Only that field of each message is kept.
func messageWorkspaceFolders(c cursorstore.Composer) []string {
	var folders []string
	seen := map[string]bool{}
	for _, b := range c.Bubbles {
		var m struct {
			WorkspaceURIs []json.RawMessage `json:"workspaceUris"`
		}
		if len(b.Value) == 0 || json.Unmarshal(b.Value, &m) != nil {
			continue
		}
		for _, raw := range m.WorkspaceURIs {
			if folder := composerWorkspaceFolder(raw); folder != "" && !seen[folder] {
				seen[folder] = true
				folders = append(folders, folder)
			}
		}
	}
	return folders
}
