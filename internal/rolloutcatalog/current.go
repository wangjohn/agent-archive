package rolloutcatalog

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
	"modernc.org/sqlite/vfs"
)

type nativeObservation struct {
	root, path string
	info       fs.FileInfo
	missing    bool
	sides      []nativeSide
}
type nativeSide struct {
	path    string
	info    fs.FileInfo
	missing bool
}

func (n nativeObservation) check() bool {
	info, err := (sourcefacts.RootOpener{Root: n.root}).Lstat(n.path)
	if n.missing {
		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	} else if err != nil || !transcriptio.SameObservation(n.info, info) {
		return false
	}
	for _, side := range n.sides {
		info, err := (sourcefacts.RootOpener{Root: n.root}).Lstat(side.path)
		if side.missing {
			if !errors.Is(err, fs.ErrNotExist) {
				return false
			}
		} else if err != nil || !transcriptio.SameObservation(side.info, info) {
			return false
		}
	}
	return true
}

// observeCurrent accepts only settled, rooted read-only databases. An indexed
// id lookup is capability-probed inside one bounded transaction. Failure makes
// current evidence unavailable, while exhaustive file lineage remains usable.
func (c *Catalog) observeCurrent(parent context.Context, root string) {
	path := filepath.Join(root, "state_5.sqlite")
	info, err := (sourcefacts.RootOpener{Root: root}).Lstat(path)
	n := nativeObservation{root: root, path: path, info: info, missing: errors.Is(err, fs.ErrNotExist)}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidePath := path + suffix
		sideInfo, sideErr := (sourcefacts.RootOpener{Root: root}).Lstat(sidePath)
		n.sides = append(n.sides, nativeSide{path: sidePath, info: sideInfo, missing: errors.Is(sideErr, fs.ErrNotExist)})
	}
	c.native = append(c.native, n)
	if err != nil || !info.Mode().IsRegular() || indexHasSides(path) {
		c.issues["current_unavailable"]++
		return
	}
	ctx, cancel := context.WithTimeout(parent, 100*time.Millisecond)
	defer cancel()
	var rootInfo fs.FileInfo
	for _, approved := range c.authorities {
		if approved.root == root {
			rootInfo = approved.info
			break
		}
	}
	fsys := &indexFS{root: root, info: rootInfo, ctx: ctx}
	name, registered, err := vfs.New(fsys)
	if err != nil {
		c.issues["current_unavailable"]++
		return
	}
	defer func() { _ = registered.Close() }()
	parameters := url.Values{"vfs": {name}, "mode": {"ro"}, "immutable": {"1"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: "state_5.sqlite", RawQuery: parameters.Encode()}).String())
	if err != nil {
		c.issues["current_unavailable"]++
		return
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	hints := map[string]agentapi.SourceRef{}
	err = func() error {
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		for _, limit := range []struct{ id, value int }{{sqlite3.SQLITE_LIMIT_LENGTH, 1 << 20}, {sqlite3.SQLITE_LIMIT_SQL_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_ATTACHED, 0}, {sqlite3.SQLITE_LIMIT_VDBE_OP, 20000}} {
			if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
				return err
			}
		}
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := c.probeSchema(ctx, tx); err != nil {
			return err
		}
		keys := make([]string, 0, len(c.threads))
		for id := range c.threads {
			keys = append(keys, id)
		}
		// A partial current hint is safe: remaining threads still require complete
		// file lineage. There is never an unbounded scan of the native table.
		if len(keys) > 1024 {
			return errors.New("native locator query budget")
		}
		for _, id := range keys {
			path, err := c.currentPath(ctx, tx, id)
			if err != nil {
				return err
			}
			if path == "" {
				continue
			}
			canonical, err := c.Files().EvalSymlinks(path)
			if err != nil {
				continue
			}
			// Match the opened, identity-validated inventory, never a DB path alone.
			for _, e := range c.files {
				if e.root == root && e.ref.Path == canonical && strings.EqualFold(e.identity.ThreadID, id) {
					ref := e.ref
					if canonical := c.rollouts[strings.ToLower(e.identity.RolloutID)]; len(canonical) == 1 {
						ref = canonical[0]
					}
					hints[id] = ref
					break
				}
			}
		}
		return tx.Commit()
	}()
	c.counters.NativeBytes += fsys.bytes
	if err != nil || !n.check() {
		c.issues["current_unavailable"]++
		return
	}
	for id, ref := range hints {
		if prior := c.current[id]; prior != nil && *prior != ref {
			c.current[id] = nil
			c.fail("current_conflict")
			continue
		}
		copy := ref
		c.current[id] = &copy
	}
}

// No VFS write or side-file operation reaches the native filesystem. The Go
// filesystem opens only the expected confined regular DB, never arbitrary
// SQLite filenames. Reads are context-aware and have a separate page-I/O cap.
type indexFS struct {
	root  string
	info  fs.FileInfo
	ctx   context.Context
	bytes int64
}

func (f *indexFS) Open(name string) (fs.File, error) {
	if name != "state_5.sqlite" {
		return nil, fs.ErrNotExist
	}
	opened, err := openAuthorityRegular(authority{root: f.root, info: f.info}, filepath.Join(f.root, name))
	if err != nil {
		return nil, err
	}
	return &indexFile{File: opened, owner: f}, nil
}

type indexFile struct {
	*os.File
	owner *indexFS
}

func (f *indexFile) Read(p []byte) (int, error) {
	if err := f.owner.ctx.Err(); err != nil {
		return 0, err
	}
	const maxIndexBytes = 4 << 20
	if f.owner.bytes >= maxIndexBytes {
		return 0, errors.New("scheduling read budget exceeded")
	}
	if int64(len(p)) > maxIndexBytes-f.owner.bytes {
		p = p[:maxIndexBytes-f.owner.bytes]
	}
	n, err := f.File.Read(p)
	f.owner.bytes += int64(n)
	return n, err
}

func indexHasSides(path string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func (c *Catalog) probeSchema(ctx context.Context, tx *sql.Tx) error {
	c.counters.NativeQueries++
	columns, err := tx.QueryContext(ctx, "PRAGMA table_info(threads)")
	if err != nil {
		return err
	}
	idOK, pathOK := false, false
	columnCount := 0
	for columns.Next() {
		var position, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := columns.Scan(&position, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			_ = columns.Close()
			return err
		}
		columnCount++
		if columnCount > 128 || len(name) > 512 || len(kind) > 128 {
			_ = columns.Close()
			return errors.New("native schema limit")
		}
		if pk > 1 {
			_ = columns.Close()
			return errors.New("unsupported native compound identity")
		}
		if name == "id" && strings.EqualFold(kind, "TEXT") && pk == 1 {
			idOK = true
		}
		if name == "rollout_path" && strings.EqualFold(kind, "TEXT") {
			pathOK = true
		}
	}
	err = errors.Join(columns.Err(), columns.Close())
	if err != nil {
		return err
	}
	if !idOK || !pathOK {
		return errors.New("native current schema unavailable")
	}
	return nil
}

func (c *Catalog) currentPath(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	const query = "SELECT substr(rollout_path,1,8193) FROM threads WHERE id = ? LIMIT 2"
	c.counters.NativeQueries++
	rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, id)
	if err != nil {
		return "", err
	}
	indexed := false
	count := 0
	for rows.Next() {
		var a, b, d int
		var detail string
		if err := rows.Scan(&a, &b, &d, &detail); err != nil {
			_ = rows.Close()
			return "", err
		}
		count++
		if count > 16 || len(detail) > 2048 {
			_ = rows.Close()
			return "", errors.New("native locator plan limit")
		}
		if strings.Contains(detail, "SEARCH threads") && strings.Contains(detail, "(id=?)") {
			indexed = true
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return "", err
	}
	if !indexed {
		return "", errors.New("native id index unavailable")
	}
	c.counters.NativeQueries++
	rows, err = tx.QueryContext(ctx, query, id)
	if err != nil {
		return "", err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return "", err
		}
		paths = append(paths, path)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return "", err
	}
	if len(paths) != 1 || len(paths[0]) > 8192 {
		return "", nil
	}
	return paths[0], nil
}
