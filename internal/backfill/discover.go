package backfill

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// transcript is one native transcript file found on disk, before resolution.
type transcript struct {
	harness string
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
	name         string
	dir, regular bool
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

// discoverClaude finds ~/.claude/projects/*/*.jsonl. The file stem is the
// native session ID.
func discoverClaude(env Environment, u *unreadable) []*transcript {
	root := filepath.Join(env.Home, ".claude", "projects")
	var found []*transcript
	for _, slug := range listStore(env, root, "claude", u) {
		if !slug.dir {
			continue
		}
		for _, f := range listDir(env, filepath.Join(root, slug.name), u) {
			if !f.regular || !strings.HasSuffix(f.name, ".jsonl") {
				continue
			}
			path := filepath.Join(root, slug.name, f.name)
			size, ok := fileSize(env, path)
			if !ok {
				continue
			}
			found = append(found, &transcript{harness: "claude", path: path, size: size, nativeID: strings.TrimSuffix(f.name, ".jsonl")})
		}
	}
	return found
}

// discoverCodex finds ~/.codex/sessions/**/rollout-*.jsonl and
// ~/.codex/archived_sessions/rollout-*.jsonl. A file in both is taken from
// sessions/.
func discoverCodex(env Environment, u *unreadable) []*transcript {
	var found []*transcript
	seen := map[string]bool{}
	var walk func(dir string, entries []dirEntry)
	walk = func(dir string, entries []dirEntry) {
		for _, e := range entries {
			path := filepath.Join(dir, e.name)
			if e.dir {
				walk(path, listDir(env, path, u))
				continue
			}
			if !e.regular || !isRolloutName(e.name) || seen[e.name] {
				continue
			}
			size, ok := fileSize(env, path)
			if !ok {
				continue
			}
			seen[e.name] = true
			found = append(found, &transcript{harness: "codex", path: path, size: size})
		}
	}
	sessions := filepath.Join(env.Home, ".codex", "sessions")
	walk(sessions, listStore(env, sessions, "codex", u))
	sessionsRead := !u.stores["codex"]
	archived := filepath.Join(env.Home, ".codex", "archived_sessions")
	archivedEntries := listStore(env, archived, "codex", u)
	u.codexArchivedOnly = sessionsRead && u.stores["codex"]
	for _, e := range archivedEntries {
		if !e.regular || !isRolloutName(e.name) || seen[e.name] {
			continue
		}
		path := filepath.Join(archived, e.name)
		size, ok := fileSize(env, path)
		if !ok {
			continue
		}
		seen[e.name] = true
		found = append(found, &transcript{harness: "codex", path: path, size: size})
	}
	return found
}

func isRolloutName(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
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
			chats[id] = &transcript{harness: "cursor", path: path, size: size, nativeID: id, cursorSlug: slug.name}
		}
		for _, id := range order {
			found = append(found, chats[id])
		}
	}
	return found
}

// readHead reads the leading records a transcript's identity and working
// directory come from. Claude Code: the cwd of the first record that has one.
// Codex: session_meta, whose payload.id is the native ID and must equal
// payload.session_id when present and the UUID in the file name.
func readHead(env Environment, t *transcript) error {
	switch t.harness {
	case "claude":
		return scanRecords(env, t.path, func(line []byte) bool {
			var r struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &r) == nil && r.Cwd != "" {
				t.cwd = r.Cwd
				return false
			}
			return true
		})
	case "codex":
		seen := 0
		metaFound := false
		err := scanRecords(env, t.path, func(line []byte) bool {
			seen++
			var r struct {
				Type      string `json:"type"`
				Timestamp string `json:"timestamp"`
				Payload   struct {
					ID        string `json:"id"`
					SessionID string `json:"session_id"`
					Timestamp string `json:"timestamp"`
					Cwd       string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &r) != nil || r.Type != "session_meta" {
				return seen < codexMetaScanLimit
			}
			metaFound = true
			t.nativeID = r.Payload.ID
			t.cwd = r.Payload.Cwd
			fileID := rolloutFileID(filepath.Base(t.path))
			if r.Payload.ID == "" || (r.Payload.SessionID != "" && r.Payload.SessionID != r.Payload.ID) || fileID == "" || !strings.EqualFold(fileID, r.Payload.ID) {
				t.identityMismatch = true
			}
			for _, ts := range []string{r.Payload.Timestamp, r.Timestamp} {
				if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					t.metaStart = parsed.UTC()
					break
				}
			}
			return false
		})
		if !metaFound {
			// Without session_meta there is no ID to register the session
			// under, so it cannot be matched with a hook registration.
			t.identityMismatch = true
		}
		return err
	}
	return nil
}

// codexMetaScanLimit bounds how far into a Codex rollout session_meta is
// looked for. Codex writes it first.
const codexMetaScanLimit = 16

// rolloutUUID is the session UUID at the end of a Codex rollout file name.
var rolloutUUID = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// rolloutFileID returns the UUID a rollout file is named with, or "".
func rolloutFileID(name string) string {
	if m := rolloutUUID.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
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
	defer f.Close()
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
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
