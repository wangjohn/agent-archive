package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
	"modernc.org/sqlite/vfs"
)

const labelIndexBytes = 4 << 20

const labelIndexRecords = 32768

var labelNativeUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// LabelContract pins native file interpretation independently of generic retained labels.
const LabelContract = "codex-files-159.2-v2"

// LabelContextVersion identifies the provider's content-free interpretation.
func (LabelProvider) LabelContextVersion() string { return LabelContract }

// LabelProvider reads only supported settled metadata for admitted sessions.
// Construction performs no I/O; live WAL and unknown storage remain unavailable.
type LabelProvider struct {
	homeLookup func(context.Context, string, []agentapi.LabelRequest) map[string]archive.SessionLabel
}

// LookupLabels shares one bounded index/DB observation per approved home.
func (p LabelProvider) LookupLabels(ctx context.Context, env agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	if env.ExternalSQLite || len(env.Homes) > 16 || len(requests) > 64 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	groups := map[string][]agentapi.LabelRequest{}
	roots := []string{}
	for _, request := range requests {
		if request.Context.Contract == "" {
			request.Context = (LabelProvider{}).LabelContext(request.Bundle)
		}
		if root, ok := labelHome(env.Homes, request); ok {
			if len(groups[root]) == 0 {
				roots = append(roots, root)
			}
			groups[root] = append(groups[root], request)
		}
	}
	// Visit homes in persistent target order so an exhausted pass rotates priority.
	for _, root := range roots {
		batch := groups[root]
		if len(batch) == 0 || ctx.Err() != nil || !labelDefaultStorage(root) {
			continue
		}
		lookup := p.homeLookup
		if lookup == nil {
			lookup = func(ctx context.Context, root string, batch []agentapi.LabelRequest) map[string]archive.SessionLabel {
				return lookupLabelHome(ctx, root, batch, slices.Contains(env.VerifiedLegacyStorageHomes, root))
			}
		}
		for id, label := range lookup(ctx, root, batch) {
			out[id] = label
		}

	}
	return out
}

func lookupLabelHome(ctx context.Context, root string, batch []agentapi.LabelRequest, legacyStorageVerified bool) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	index, indexComplete := readLabelIndex(ctx, root, batch)
	rows, settled, absent := readLabelDatabase(ctx, root, batch)
	if !settled {
		return out
	}
	for _, request := range batch {
		row, found := rows[request.Registration.NativeSessionID]
		if filtered, ok := resolveLabel(root, request, row, found, absent && legacyStorageVerified, index, indexComplete); ok {
			out[request.Registration.ArchiveSessionID] = filtered
		}
	}
	return out
}

func resolveLabel(root string, request agentapi.LabelRequest, row labelRow, found, absent bool, index map[string]string, indexComplete bool) (archive.SessionLabel, bool) {
	reg := request.Registration
	var name string
	source := archive.SessionLabelIndex
	if found {
		if row.version != "0.159.2" || !labelOwnedPath(root, reg.TranscriptPath, row.path) {
			return archive.SessionLabel{}, false
		}
		source = archive.SessionLabelDatabase
		switch row.mode {
		case codexmeta.CodexHistoryPaginated:
			name = strings.TrimSpace(row.name)
		case codexmeta.CodexHistoryLegacy:
			title := strings.TrimSpace(row.title)
			if title != "" && title != strings.TrimSpace(row.first) {
				// Guardian's derived default requires native source classification.
				if strings.Contains(row.source, "guardian") {
					return archive.SessionLabel{}, false
				}
				name = title
			} else {
				if !indexComplete {
					return archive.SessionLabel{}, false
				}
				name = index[strings.ToLower(reg.NativeSessionID)]
			}
			if strings.TrimSpace(name) == strings.TrimSpace(row.preview) {
				name = ""
			}
		default:
			return archive.SessionLabel{}, false
		}
	} else {
		// A DB with no matching row is not proof of index-only authority.
		if !absent || !indexComplete || !request.Context.Legacy || request.Context.Producer != "0.159.2" || !request.Context.PreviewComplete {
			return archive.SessionLabel{}, false
		}
		name = index[strings.ToLower(reg.NativeSessionID)]
		if labelPreviewDigest(name) == request.Context.PreviewDigest {
			name = ""
		}
	}
	state := archive.SessionLabelAbsent
	if strings.TrimSpace(name) != "" {
		state = archive.SessionLabelPresent
	}
	label := archive.SessionLabel{NativeID: reg.NativeSessionID, State: state, Name: name, Source: source, Contract: LabelContract}
	return archive.FilterSessionLabel(label)
}

func labelHome(homes []string, request agentapi.LabelRequest) (string, bool) {
	r, proof := request.Registration, request.Context
	if r.Harness.Name != "codex" || r.CaptureFrozen || r.Imported() || !proof.Ordinary || proof.Producer != "0.159.2" || !labelNativeUUID.MatchString(r.NativeSessionID) || proof.NativeID != r.NativeSessionID {
		return "", false
	}
	matched := ""
	for _, root := range homes {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			continue
		}
		if !labelOwnedPath(root, r.TranscriptPath, r.TranscriptPath) || (r.DiscoveryRoot != "" && r.DiscoveryRoot != root) {
			continue
		}
		if matched != "" {
			return "", false
		}
		matched = root
	}
	return matched, matched != ""
}

func labelOwnedPath(root, admitted, observed string) bool {
	if admitted == "" || admitted != observed || !filepath.IsAbs(admitted) || filepath.Clean(admitted) != admitted {
		return false
	}
	return local.PathWithin(admitted, filepath.Join(root, "sessions")) || local.PathWithin(admitted, filepath.Join(root, "archived_sessions"))
}

// Unknown quoting/profiles/placement are rejected rather than partly interpreting TOML.
func labelDefaultStorage(root string) bool {
	path := filepath.Join(root, "config.toml")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return true
	}
	f, err := labelOpenRegular(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 {
		return false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "sqlite_home") || strings.Contains(line, "profiles") || strings.HasPrefix(line, "profile ") || strings.HasPrefix(line, "profile=") || strings.HasPrefix(line, "\"") || strings.HasPrefix(line, "'") || strings.Contains(strings.SplitN(line, "=", 2)[0], "\\") {
			return false
		}
	}
	return true
}

func readLabelIndex(ctx context.Context, root string, requests []agentapi.LabelRequest) (map[string]string, bool) {
	out := map[string]string{}
	wanted := map[string]bool{}
	for _, request := range requests {
		wanted[strings.ToLower(request.Registration.NativeSessionID)] = true
	}
	path := filepath.Join(root, "session_index.jsonl")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return out, true
	}
	f, err := labelOpenRegular(root, path)
	if errors.Is(err, os.ErrNotExist) {
		return out, true
	}
	if err != nil {
		return out, false
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil || before.Size() > labelIndexBytes {
		return out, false
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, labelIndexBytes+1), 16384)
	for count := 0; ; count++ {
		if count >= labelIndexRecords || ctx.Err() != nil {
			return nil, false
		}
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			break
		}
		// Incomplete tails and overlong lines cannot establish absence or freshness.
		if err != nil {
			return nil, false
		}
		var entry struct {
			ID      string  `json:"id"`
			Name    string  `json:"thread_name"`
			Updated *string `json:"updated_at"`
		}
		if json.Unmarshal(line, &entry) != nil || !wanted[strings.ToLower(entry.ID)] || strings.TrimSpace(entry.Name) == "" || entry.Updated == nil {
			continue
		}
		out[strings.ToLower(entry.ID)] = strings.TrimSpace(entry.Name)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, false
	}
	return out, true
}

type labelRow struct {
	mode    codexmeta.HistoryMode
	name    string
	title   string
	first   string
	preview string
	source  string
	version string
	path    string
}

func readLabelDatabase(ctx context.Context, root string, requests []agentapi.LabelRequest) (map[string]labelRow, bool, bool) {
	out := map[string]labelRow{}
	path := filepath.Join(root, "state_5.sqlite")
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, !labelSides(path), true
	}
	if err != nil || !before.Mode().IsRegular() || labelSides(path) {
		return nil, false, false
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	header, ok := labelDBHeader(root)
	if !ok {
		return nil, false, false
	}
	fsys := &labelFS{root: root, ctx: ctx, expected: before}
	name, registered, err := vfs.New(fsys)
	if err != nil {
		return nil, false, false
	}
	defer func() { _ = registered.Close() }()
	query := url.Values{"vfs": {name}, "mode": {"ro"}, "immutable": {"1"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: "state_5.sqlite", RawQuery: query.Encode()}).String())
	if err != nil {
		return nil, false, false
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, false
	}
	defer func() { _ = conn.Close() }()
	for _, limit := range []struct {
		id    int
		value int
	}{{sqlite3.SQLITE_LIMIT_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_SQL_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_ATTACHED, 0}, {sqlite3.SQLITE_LIMIT_VDBE_OP, 20000}} {
		if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
			return nil, false, false
		}
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, false
	}
	defer func() { _ = tx.Rollback() }()
	if !labelDatabaseSchema(ctx, tx) {
		return nil, false, false
	}
	// The primary-key plan and explicit columns refuse incompatible schemas and scans.
	const statement = "SELECT history_mode,name,title,first_user_message,preview,source,cli_version,rollout_path FROM threads WHERE id=?"
	plan, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+statement, "")
	if err != nil {
		return nil, false, false
	}
	defer func() { _ = plan.Close() }()
	indexed := false
	for plan.Next() {
		var a, b, c int
		var detail string
		if plan.Scan(&a, &b, &c, &detail) != nil {
			_ = plan.Close()
			return nil, false, false
		}
		if strings.Contains(detail, "SEARCH threads USING INDEX sqlite_autoindex_threads_1 (id=?)") {
			indexed = true
		}
	}
	planErr := plan.Err()
	_ = plan.Close()
	if planErr != nil || !indexed {
		return nil, false, false
	}
	for _, request := range requests {
		var values [8]any
		err := tx.QueryRowContext(ctx, statement, request.Registration.NativeSessionID).Scan(&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6], &values[7])
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, false, false
		}
		row, ok := labelDatabaseRow(values)
		if !ok {
			return nil, false, false
		}
		out[request.Registration.NativeSessionID] = row
	}
	after, err := os.Lstat(path)
	headerAfter, headerOK := labelDBHeader(root)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || labelSides(path) || !headerOK || !bytes.Equal(header, headerAfter) || !labelDefaultStorage(root) {
		return nil, false, false
	}
	return out, true, false
}

func labelDatabaseSchema(ctx context.Context, tx *sql.Tx) bool {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(threads)")
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	required := map[string]bool{"id": false, "history_mode": false, "name": false, "title": false, "first_user_message": false, "preview": false, "source": false, "cli_version": false, "rollout_path": false}
	for rows.Next() {
		var ordinal, notNull, primary int
		var name, kind string
		var defaultValue any
		if rows.Scan(&ordinal, &name, &kind, &notNull, &defaultValue, &primary) != nil {
			return false
		}
		if _, needed := required[name]; needed {
			if kind != "TEXT" || (name == "id" && primary != 1) {
				return false
			}
			required[name] = true
		}
	}
	if rows.Err() != nil {
		return false
	}
	for _, found := range required {
		if !found {
			return false
		}
	}
	return true
}

func labelDatabaseRow(values [8]any) (labelRow, bool) {
	var text [8]string
	for i, value := range values {
		if value == nil && (i == 1 || i == 3) {
			continue
		}
		var ok bool
		if text[i], ok = value.(string); !ok {
			return labelRow{}, false
		}
	}
	return labelRow{mode: codexmeta.HistoryMode(text[0]), name: text[1], title: text[2], first: text[3], preview: text[4], source: text[5], version: text[6], path: text[7]}, true
}

func labelSides(path string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// The VFS receives only bounded reads of the confined DB; it has no native write path.
type labelFS struct {
	root     string
	ctx      context.Context
	bytes    int64
	expected os.FileInfo
}

func (f *labelFS) Open(name string) (fs.File, error) {
	if name != "state_5.sqlite" {
		return nil, fs.ErrNotExist
	}
	file, err := labelOpenRegular(f.root, filepath.Join(f.root, name))
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(f.expected, info) {
		_ = file.Close()
		return nil, errors.New("label database changed while opening")
	}
	return &labelFile{File: file, owner: f}, nil
}

type labelFile struct {
	*os.File
	owner *labelFS
}

func (f *labelFile) Read(p []byte) (int, error) {
	if err := f.owner.ctx.Err(); err != nil {
		return 0, err
	}
	const budget = 8 << 20
	if f.owner.bytes >= budget {
		return 0, errors.New("label database read budget exceeded")
	}
	if int64(len(p)) > budget-f.owner.bytes {
		p = p[:budget-f.owner.bytes]
	}
	n, err := f.File.Read(p)
	f.owner.bytes += int64(n)
	return n, err
}

func labelDBHeader(root string) ([]byte, bool) {
	f, err := labelOpenRegular(root, filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	header := make([]byte, 100)
	_, err = io.ReadFull(f, header)
	return header, err == nil
}

// LabelContext derives safe content-free equality facts once from retained source.
func (LabelProvider) LabelContext(bundle archive.SourceBundle) agentapi.LabelContext {
	proof := agentapi.LabelContext{NativeID: bundle.NativeSessionID, Producer: bundle.Capture.Harness.Version, Contract: LabelContract}
	if bundle.History != nil || bundle.SchemaVersion != archive.SourceSchemaVersion || bundle.Capture.Harness.Name != "codex" {
		return proof
	}
	owned := false
	preview := ""
	for _, record := range bundle.NativeRecords {
		payload, _ := record["payload"].(map[string]any)
		if record["type"] == "session_meta" {
			if payload["id"] != bundle.NativeSessionID {
				return proof
			}
			owned = true
			mode, explicit := payload["history_mode"]
			proof.Legacy = mode == "legacy" || (!explicit && bundle.Capture.FilterVersion == archive.FilterVersion)
		}
		if record["type"] == "event_msg" && payload["type"] == "user_message" {
			if value, ok := payload["message"].(string); ok {
				preview = value
				break
			}
		}
	}
	proof.Ordinary = owned
	proof.PreviewComplete = owned && len(bundle.Capture.Gaps) == 0
	proof.PreviewDigest = labelPreviewDigest(preview)
	return proof
}

func labelPreviewDigest(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

// labelOpenRegular confines native reads to an approved root and never blocks
// on a FIFO. It intentionally has no higher-level preview/discovery dependency.
func labelOpenRegular(root, path string) (*os.File, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, errors.New("invalid label source locator")
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("label source root unavailable")
	}
	defer func() { _ = directory.Close() }()
	before, err := directory.Lstat(rel)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("label source is not a regular file")
	}
	file, err := directory.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("label source unavailable")
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, errors.New("label source changed while opening")
	}
	return file, nil
}

func supportedLabelContract(contract string) bool {
	return contract == LabelContract || contract == "codex-files-159.2-v1"
}

// LabelRequestGroup shares lookup priority for targets in one verified native home.
func (p LabelProvider) LabelRequestGroup(env agentapi.LabelEnvironment, request agentapi.LabelRequest) string {
	if request.Context.Contract == "" {
		request.Context = p.LabelContext(request.Bundle)
	}
	root, ok := labelHome(env.Homes, request)
	if !ok {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])
}
