package backfill

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/nativesessions"
)

// transcript is one native transcript file found on disk, before resolution.
type transcript struct {
	harness harness
	path    string
	size    int64
	// nativeID is the ID the session is registered under: the Claude Code
	// file stem, Codex's session_meta.payload.id, or the Cursor chat folder.
	nativeID string
	// cursorSlug is the Cursor project folder the chat was found in.
	cursorSlug string
	// cwd is the working directory the transcript records (Claude Code and
	// Codex).
	cwd string
	// identityMismatch is set when the transcript's own IDs disagree.
	identityMismatch bool
	// metaStart is Codex's session_meta timestamp.
	metaStart time.Time
}

// unreadable records what discovery could not list. Paths are never kept.
type unreadable struct {
	// folders counts folders inside an app's store.
	folders int
	// stores are the apps whose store root could not be listed, so none, or
	// for Codex's archived_sessions only the archived, of their sessions were
	// found.
	stores map[string]bool
	// codexArchivedOnly is set when Codex's archived_sessions could not be
	// listed but its sessions folder could.
	codexArchivedOnly bool
	// cursorIncomplete is set when any part of Cursor's transcript store
	// could not be listed, so a chat's transcript may exist unseen.
	cursorIncomplete bool
}

// discover lists every transcript file in the three apps' default stores. It
// only lists directories; nothing is opened here. A folder that cannot be
// listed is passed over and recorded in u, so one bad folder never stops the
// plan, and its path is never shown.
func discover(env Environment) (found []*transcript, u unreadable) {
	u.stores = map[string]bool{}
	found = append(found, discoverClaude(env, &u)...)
	found = append(found, discoverCodex(env, &u)...)
	found = append(found, discoverCursor(env, &u)...)
	return found, u
}

// listDir lists a folder inside an app's store. A missing folder, or a path
// that is not a folder, is empty; one that cannot be read is counted and
// treated as empty.
func listDir(env Environment, dir string, u *unreadable) []dirEntry {
	entries, ok := tryList(env, dir)
	if !ok {
		u.folders++
	}
	return entries
}

// listStore lists the root of app's store, recording app when it cannot be
// read: then none of the sessions below it are found.
func listStore(env Environment, dir, app string, u *unreadable) []dirEntry {
	entries, ok := tryList(env, dir)
	if !ok {
		u.stores[app] = true
	}
	return entries
}

// tryList lists dir; ok is false only when it exists as a folder and cannot
// be read.
func tryList(env Environment, dir string) ([]dirEntry, bool) {
	entries, err := readDirIfExists(env, dir)
	if err != nil {
		return nil, errors.Is(err, syscall.ENOTDIR)
	}
	return entries, true
}

// readDirIfExists lists dir, treating a missing directory as empty.
func readDirIfExists(env Environment, dir string) ([]dirEntry, error) {
	entries, err := env.readDir(dir)
	if err != nil {
		if isNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, dirEntry{name: e.Name(), dir: e.IsDir(), regular: e.Type().IsRegular()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

type dirEntry struct {
	name    string
	dir     bool
	regular bool
}

// fileSize stats a regular file without following symlinks; ok is false when
// it is gone or is not one. A symlinked transcript is skipped: discovery
// never follows a link out of an app's store.
func fileSize(env Environment, path string) (int64, bool) {
	info, err := env.lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	return info.Size(), true
}

// discoverClaude finds <dir>/projects/*/*.jsonl in each of Claude Code's
// folders (~/.claude, and $CLAUDE_CONFIG_DIR). The file stem is the native
// session ID; a session found in two folders is a duplicate_session.
func discoverClaude(env Environment, u *unreadable) []*transcript {
	var found []*transcript
	for _, dir := range env.claudeDirs() {
		found = append(found, discoverClaudeIn(env, filepath.Join(dir, "projects"), u)...)
	}
	return found
}

type nativeDirectories struct{ env Environment }

func (n nativeDirectories) ReadDir(path string) ([]fs.DirEntry, error) { return n.env.readDir(path) }

func discoverClaudeIn(env Environment, root string, u *unreadable) []*transcript {
	var found []*transcript
	coverage, _ := nativesessions.Walk(context.Background(), nativeDirectories{env}, nativesessions.StoreRoot{Harness: "claude", Path: root}, 0, func(ref nativesessions.Ref) (bool, error) {
		size, ok := fileSize(env, ref.Path)
		if ok {
			found = append(found, &transcript{harness: harnessClaude, path: ref.Path, size: size, nativeID: strings.TrimSuffix(filepath.Base(ref.Path), ".jsonl")})
		}
		return true, nil
	})
	u.folders += coverage.UnreadableFolders
	if coverage.RootUnreadable {
		u.stores["claude"] = true
	}
	return found
}

// discoverCodex finds <dir>/sessions/**/rollout-*.jsonl and
// <dir>/archived_sessions/rollout-*.jsonl in each of Codex's folders
// (~/.codex, and $CODEX_HOME). A file in both is taken from sessions/.
func discoverCodex(env Environment, u *unreadable) []*transcript {
	var found []*transcript
	seen := map[string]bool{}
	// Each folder is judged on its own; the plan then says whether any
	// sessions folder, or only archived ones, could not be read.
	storeUnread, sessionsUnread := false, false
	for _, dir := range env.codexDirs() {
		u.stores["codex"], u.codexArchivedOnly = false, false
		found = append(found, discoverCodexIn(env, dir, seen, u)...)
		if u.stores["codex"] {
			storeUnread = true
			sessionsUnread = sessionsUnread || !u.codexArchivedOnly
		}
	}
	u.stores["codex"] = storeUnread
	u.codexArchivedOnly = storeUnread && !sessionsUnread
	return found
}

func discoverCodexIn(env Environment, codexDir string, seen map[string]bool, u *unreadable) []*transcript {
	var found []*transcript
	for index, store := range []string{"sessions", "archived_sessions"} {
		coverage, _ := nativesessions.Walk(context.Background(), nativeDirectories{env}, nativesessions.StoreRoot{Harness: "codex", Path: filepath.Join(codexDir, store), Recursive: index == 0}, 0, func(ref nativesessions.Ref) (bool, error) {
			name := filepath.Base(ref.Path)
			if seen[name] {
				return true, nil
			}
			size, ok := fileSize(env, ref.Path)
			if ok {
				seen[name] = true
				found = append(found, &transcript{harness: harnessCodex, path: ref.Path, size: size})
			}
			return true, nil
		})
		u.folders += coverage.UnreadableFolders
		if coverage.RootUnreadable {
			if index == 1 {
				u.codexArchivedOnly = !u.stores["codex"]
			}
			u.stores["codex"] = true
		}
	}
	return found
}

// discoverCursor finds ~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl,
// plus the text form older Cursor versions wrote beside them,
// agent-transcripts/<id>.txt. The collector's Cursor filter reads either
// (collector.FilterTranscriptFile falls back to the text filter). The folder
// name is the chat ID hooks register; the JSONL file wins when a chat has
// both.
func discoverCursor(env Environment, u *unreadable) []*transcript {
	root := filepath.Join(env.Home, ".cursor", "projects")
	var found []*transcript
	unreadBefore := u.folders
	defer func() { u.cursorIncomplete = u.stores["cursor"] || u.folders > unreadBefore }()
	for _, slug := range listStore(env, root, "cursor", u) {
		if !slug.dir {
			continue
		}
		dir := filepath.Join(root, slug.name, "agent-transcripts")
		chats := map[string]*transcript{}
		var order []string
		for _, e := range listDir(env, dir, u) {
			var id, path string
			switch {
			case e.dir:
				id, path = e.name, filepath.Join(dir, e.name, e.name+".jsonl")
			case e.regular && strings.HasSuffix(e.name, ".txt"):
				id, path = strings.TrimSuffix(e.name, ".txt"), filepath.Join(dir, e.name)
			default:
				continue
			}
			if id == "" {
				continue
			}
			size, ok := fileSize(env, path)
			if !ok {
				continue
			}
			if existing, dup := chats[id]; dup {
				if strings.HasSuffix(existing.path, ".jsonl") {
					continue
				}
			} else {
				order = append(order, id)
			}
			chats[id] = &transcript{harness: harnessCursor, path: path, size: size, nativeID: id, cursorSlug: slug.name}
		}
		for _, id := range order {
			found = append(found, chats[id])
		}
	}
	return found
}

// readHead preserves import's 8 MiB scan and 16-record Codex rule.
func readHead(env Environment, t *transcript) error {
	if t.harness == harnessCursor {
		return nil
	}
	h, err := nativesessions.Inspect(string(t.harness), t.path, func(visit func([]byte) bool) error { return scanRecords(env, t.path, visit) })
	t.cwd = h.Directory
	t.nativeID = h.NativeID
	t.metaStart = h.StartedAt
	t.identityMismatch = h.IdentityMismatch
	return err
}

// headLineLimit is the longest line the header scan decodes; a longer one is
// passed over, since it cannot be a header. headScanLimit bounds how far into
// a file the scan reads. Both keep header reads small, since they run outside
// the filter's byte budget.
const (
	headLineLimit = 1 << 20
	headScanLimit = 8 << 20
)

// scanRecords calls visit with each non-blank line until it returns false,
// reading at most headScanLimit bytes. Lines over headLineLimit are skipped
// without being held in memory.
func scanRecords(env Environment, path string, visit func([]byte) bool) error {
	f, err := env.open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	reader := bufio.NewReaderSize(io.LimitReader(f, headScanLimit), 64*1024)
	var line []byte
	tooLong := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > headLineLimit {
				tooLong, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if trimmed := bytes.TrimSpace(line); !tooLong && len(trimmed) > 0 && !visit(trimmed) {
			return nil
		}
		line, tooLong = line[:0], false
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
