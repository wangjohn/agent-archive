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
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
	"modernc.org/sqlite/vfs"
)

const labelIndexBytes = 4 << 20
const labelIndexRecords = 32768

// LabelProvider reads only supported settled metadata for admitted sessions.
// Construction performs no I/O; live WAL and unknown storage remain unavailable.
type LabelProvider struct{}

// LookupLabels shares one bounded index/DB observation per approved home.
func (LabelProvider) LookupLabels(ctx context.Context, env agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	out := map[string]archive.SessionLabel{}
	if env.ExternalSQLite || len(env.Homes) > 16 || len(requests) > 64 {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	groups := map[string][]agentapi.LabelRequest{}
	for _, request := range requests {
		if request.Context.Contract == "" {
			request.Context = (LabelProvider{}).LabelContext(request.Bundle)
		}
		if root, ok := labelHome(env.Homes, request); ok {
			groups[root] = append(groups[root], request)
		}
	}
	// Configured order is stable; the collector's persistent request cursor supplies fairness.
	for _, root := range env.Homes {
		batch := groups[root]
		if len(batch) == 0 || ctx.Err() != nil || !labelDefaultStorage(root) {
			continue
		}
		index, indexComplete := readLabelIndex(ctx, root, batch)
		rows, settled, absent := readLabelDatabase(ctx, root, batch)
		if !settled {
			continue
		}
		for _, request := range batch {
			reg := request.Registration
			row, found := rows[reg.NativeSessionID]
			if filtered, ok := resolveLabel(root, request, row, found, absent, index, indexComplete); ok {
				out[reg.ArchiveSessionID] = filtered
			}
		}
	}
	return out
}

func resolveLabel(root string, request agentapi.LabelRequest, row labelRow, found, absent bool, index map[string]string, indexComplete bool) (archive.SessionLabel, bool) {
	reg := request.Registration
	name, source := "", "index"
	if found {
		if row.version != "0.159.2" || !labelOwnedPath(root, reg.TranscriptPath, row.path) {
			return archive.SessionLabel{}, false
		}
		source = "database"
		switch row.mode {
		case "paginated":
			name = strings.TrimSpace(row.name)
		case "legacy":
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
				name = index[reg.NativeSessionID]
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
		name = index[reg.NativeSessionID]
		if labelPreviewDigest(name) == request.Context.PreviewDigest {
			name = ""
		}
	}
	label := archive.SessionLabel{NativeID: reg.NativeSessionID, State: "confirmed_absent", Source: source, Contract: archive.SessionLabelContract}
	if strings.TrimSpace(name) != "" {
		label.State, label.Name = "present", name
	}
	return archive.FilterSessionLabel(label)
}

func labelHome(homes []string, request agentapi.LabelRequest) (string, bool) {
	r, proof := request.Registration, request.Context
	if r.Harness.Name != "codex" || r.CaptureFrozen || r.Imported() || !proof.Ordinary || proof.NativeID != r.NativeSessionID {
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
	f, err := sourcefacts.OpenRegular(root, path)
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
	for _, line := range strings.Split(string(b), "\n") {
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
		wanted[request.Registration.NativeSessionID] = true
	}
	path := filepath.Join(root, "session_index.jsonl")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return out, true
	}
	f, err := sourcefacts.OpenRegular(root, path)
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
		if err == io.EOF && len(line) == 0 {
			break
		}
		// Incomplete tails and overlong lines cannot establish absence or freshness.
		if err != nil {
			return nil, false
		}
		var entry struct {
			ID      string `json:"id"`
			Name    string `json:"thread_name"`
			Updated string `json:"updated_at"`
		}
		if json.Unmarshal(line, &entry) != nil || !wanted[entry.ID] || strings.TrimSpace(entry.Name) == "" || entry.Updated == "" {
			continue
		}
		out[entry.ID] = strings.TrimSpace(entry.Name)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, false
	}
	return out, true
}

type labelRow struct{ mode, name, title, first, preview, source, version, path string }

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
	for _, limit := range []struct{ id, value int }{{sqlite3.SQLITE_LIMIT_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_SQL_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_ATTACHED, 0}, {sqlite3.SQLITE_LIMIT_VDBE_OP, 20000}} {
		if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
			return nil, false, false
		}
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, false
	}
	defer func() { _ = tx.Rollback() }()
	// The primary-key plan and explicit columns refuse incompatible schemas and scans.
	const statement = "SELECT history_mode,coalesce(name,''),title,coalesce(first_user_message,''),preview,source,cli_version,rollout_path FROM threads WHERE id=?"
	plan, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+statement, "")
	if err != nil {
		return nil, false, false
	}
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
		var row labelRow
		err := tx.QueryRowContext(ctx, statement, request.Registration.NativeSessionID).Scan(&row.mode, &row.name, &row.title, &row.first, &row.preview, &row.source, &row.version, &row.path)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
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
	file, err := sourcefacts.OpenRegular(f.root, filepath.Join(f.root, name))
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
	f, err := sourcefacts.OpenRegular(root, filepath.Join(root, "state_5.sqlite"))
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
	proof := agentapi.LabelContext{NativeID: bundle.NativeSessionID, Producer: bundle.Capture.Harness.Version, Contract: archive.SessionLabelContract}
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
