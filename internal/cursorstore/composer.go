package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Composer is one Cursor chat as read from state.vscdb: its composerData
// value and its messages in header order. A message whose row is missing has
// a nil Value. Its shape is archive.CursorComposer's, field for field.
type Composer struct {
	Composer json.RawMessage
	Bubbles  []Bubble
}

// Bubble is one message of a chat: its bubble ID and its bubbleId row's
// value, nil when the row is missing.
type Bubble struct {
	ID    string
	Value json.RawMessage
}

// Signature is what tells one state of a chat from the next without reading
// its messages (spec, phase 2 decision 7): the database file changes
// constantly, so its stat can't. It comes from the composerData value alone.
type Signature struct {
	// LastUpdatedAt is composerData.lastUpdatedAt in Unix milliseconds, zero
	// when absent.
	LastUpdatedAt int64 `json:"last_updated_at,omitempty"`
	// HeaderCount is how many messages the chat lists.
	HeaderCount int `json:"header_count"`
	// LastBubbleID is the last listed message's ID.
	LastBubbleID string `json:"last_bubble_id,omitempty"`
}

// ErrComposerNotFound means the database has no composerData row for the
// chat: Cursor deleted it. It wraps fs.ErrNotExist.
var ErrComposerNotFound = fmt.Errorf("no such Cursor chat: %w", fs.ErrNotExist)

// snapshotPrefix names the private directories that hold database copies.
const snapshotPrefix = "cursor-snapshot-"

// staleSnapshotAge is how old a leftover snapshot directory must be before a
// later read removes it. Only a killed process leaves one.
const staleSnapshotAge = time.Hour

// afterSnapshot, when set by a test, runs once the copy is written and
// before it is read.
var afterSnapshot func(copyPath string)

// ReadComposer reads one chat consistently: its composerData row and its
// bubbleId:<composerID>:<bubbleID> rows, in header order.
//
// With Cursor running, the database is first copied with SQLite's online
// backup API (spec, phase 2 decision 3), from a read-only connection opened
// as Read opens it, into a 0600 file in a new private directory under
// scratchDir; the copy is read, and the directory is removed on every path,
// including a panic. The backup takes one read transaction over the whole
// database, so the copy is one committed state even while Cursor writes; a
// lock held past the timeout fails the read rather than reading partially.
// With Cursor closed, the backup would create -wal beside the source, so the
// database is read in place as Read does, immutable and checked afterwards.
//
// A missing database is ErrNoDatabase and a missing chat
// ErrComposerNotFound; both wrap fs.ErrNotExist. Anything else that can't be
// read safely is a *NotCheckedError.
func ReadComposer(ctx context.Context, dbPath, composerID, scratchDir string) (Composer, Signature, error) {
	if composerID == "" {
		return Composer{}, Signature{}, errors.New("a Cursor composer ID is required")
	}
	var c Composer
	var sig Signature
	read := func(ctx context.Context, db *sql.DB) error {
		var err error
		c, sig, err = queryComposer(ctx, db, composerID)
		return err
	}
	src, err := resolve(dbPath)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	if !src.live {
		err = readInPlace(ctx, src, Options{}, read)
	} else {
		err = readSnapshot(ctx, src, scratchDir, read)
	}
	if err != nil {
		return Composer{}, Signature{}, err
	}
	return c, sig, nil
}

// ReadSignature reads only the chat's composerData row, in place as Read
// does, and returns the Signature ReadComposer would return for the same
// state. One statement sees one committed state, so it needs no snapshot,
// and it costs one indexed row rather than a copy of the database.
func ReadSignature(ctx context.Context, dbPath, composerID string) (Signature, error) {
	if composerID == "" {
		return Signature{}, errors.New("a Cursor composer ID is required")
	}
	var sig Signature
	err := Read(ctx, dbPath, Options{}, func(ctx context.Context, db *sql.DB) error {
		value, err := composerRow(ctx, db, composerID)
		if err != nil {
			return err
		}
		sig, _, err = decodeHeaders(value)
		return err
	})
	if err != nil {
		return Signature{}, err
	}
	return sig, nil
}

// readSnapshot copies the live database src into a private directory under
// scratchDir with the online backup API, calls read on the copy, and removes
// the directory whatever happens.
func readSnapshot(ctx context.Context, src source, scratchDir string, read func(context.Context, *sql.DB) error) (err error) {
	if scratchDir == "" {
		return errors.New("a directory for the Cursor database snapshot is required")
	}
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	removeStaleSnapshots(scratchDir)
	// MkdirTemp creates the directory 0700.
	dir, err := os.MkdirTemp(scratchDir, snapshotPrefix)
	if err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && err == nil {
			err = fmt.Errorf("remove Cursor database snapshot: %w", rmErr)
		}
	}()
	copyPath := filepath.Join(dir, "state.vscdb")
	// Created here, empty and 0600, so SQLite opens it rather than creating
	// it with the default mode.
	f, err := os.OpenFile(copyPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create snapshot file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("create snapshot file: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	if err := backup(ctx, dsn(src.path, true), copyPath); err != nil {
		return notChecked(err)
	}
	if afterSnapshot != nil {
		afterSnapshot(copyPath)
	}
	// The copy is this process's own and nothing writes it, so immutable is
	// exact, and it opens no side file even though the copy's header still
	// says WAL.
	if err := query(ctx, dsn(copyPath, false), read); err != nil {
		return notChecked(err)
	}
	return nil
}

// backuper is the modernc.org/sqlite connection's online backup API.
type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

// backupRetry is how long backup waits before retrying a busy source.
const backupRetry = 25 * time.Millisecond

// backup copies the database srcDSN opens into the file at dst with SQLite's
// online backup API, in one step so the copy is one read transaction's
// state. SQLITE_BUSY and SQLITE_LOCKED are retried until ctx ends.
func backup(ctx context.Context, srcDSN, dst string) error {
	db, err := sql.Open("sqlite", srcDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Opened read-write: the copy's own rollback journal, if SQLite makes
	// one, lands in the private directory beside it.
	dstURI := (&url.URL{Scheme: "file", Path: dst}).String()
	return conn.Raw(func(driverConn any) error {
		b, ok := driverConn.(backuper)
		if !ok {
			return errors.New("the SQLite driver has no online backup API")
		}
		for {
			bk, err := b.NewBackup(dstURI)
			if err != nil {
				return err
			}
			_, stepErr := bk.Step(-1)
			finishErr := bk.Finish()
			if stepErr == nil {
				return finishErr
			}
			if !busy(stepErr) {
				return stepErr
			}
			select {
			case <-ctx.Done():
				return &NotCheckedError{Reason: Locked, Err: stepErr}
			case <-time.After(backupRetry):
			}
		}
	})
}

func busy(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	return false
}

// removeStaleSnapshots removes snapshot directories a killed process left in
// scratchDir, so a copy of Cursor's chats does not outlive its read.
func removeStaleSnapshots(scratchDir string) {
	entries, err := os.ReadDir(scratchDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), snapshotPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleSnapshotAge {
			os.RemoveAll(filepath.Join(scratchDir, e.Name()))
		}
	}
}

// composerRow reads the chat's composerData value.
func composerRow(ctx context.Context, db *sql.DB, composerID string) ([]byte, error) {
	var value []byte
	err := db.QueryRowContext(ctx, `SELECT value FROM cursorDiskKV WHERE key = ?`, "composerData:"+composerID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && value == nil) {
		return nil, ErrComposerNotFound
	}
	return value, err
}

// bubbleQuery reads one chat's message rows through the key index. The
// bounds are the prefix bubbleId:<composerID>: and that prefix with its
// final ':' raised to ';', which bracket exactly the keys with the prefix.
const bubbleQuery = `SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ?`

// queryComposer reads the chat's composerData row, then its message rows.
// Within a snapshot or an unchanged immutable file the two statements see
// the same state.
func queryComposer(ctx context.Context, db *sql.DB, composerID string) (Composer, Signature, error) {
	value, err := composerRow(ctx, db, composerID)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	sig, ids, err := decodeHeaders(value)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	c := Composer{Composer: json.RawMessage(value), Bubbles: make([]Bubble, len(ids))}
	if len(ids) == 0 {
		return c, sig, nil
	}
	prefix := "bubbleId:" + composerID + ":"
	rows, err := db.QueryContext(ctx, bubbleQuery, prefix, prefix[:len(prefix)-1]+";")
	if err != nil {
		return Composer{}, Signature{}, err
	}
	defer rows.Close()
	found := map[string]json.RawMessage{}
	for rows.Next() {
		var key string
		var v []byte
		if err := rows.Scan(&key, &v); err != nil {
			return Composer{}, Signature{}, err
		}
		if v != nil {
			found[strings.TrimPrefix(key, prefix)] = v
		}
	}
	if err := rows.Err(); err != nil {
		return Composer{}, Signature{}, err
	}
	for i, id := range ids {
		c.Bubbles[i] = Bubble{ID: id, Value: found[id]}
	}
	return c, sig, nil
}

// decodeHeaders reads the chat's Signature and its message IDs in order from
// its composerData value: fullConversationHeadersOnly, or in older chats the
// inline conversation. An older chat's messages are inline, so it lists no
// message rows to read. A value that is not an object, or a message list or
// ID of another shape, is UnknownFormat.
func decodeHeaders(value []byte) (Signature, []string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil || fields == nil {
		return Signature{}, nil, NotChecked(UnknownFormat)
	}
	present := func(name string) (json.RawMessage, bool) {
		raw, ok := fields[name]
		return raw, ok && string(raw) != "null"
	}
	var sig Signature
	if raw, ok := present("lastUpdatedAt"); ok {
		var ms float64
		if json.Unmarshal(raw, &ms) != nil {
			return Signature{}, nil, NotChecked(UnknownFormat)
		}
		sig.LastUpdatedAt = int64(ms)
	}
	type header struct {
		BubbleID *string `json:"bubbleId"`
	}
	var ids []string
	if raw, ok := present("fullConversationHeadersOnly"); ok {
		var headers []header
		if json.Unmarshal(raw, &headers) != nil {
			return Signature{}, nil, NotChecked(UnknownFormat)
		}
		for _, h := range headers {
			if h.BubbleID == nil || *h.BubbleID == "" {
				return Signature{}, nil, NotChecked(UnknownFormat)
			}
			ids = append(ids, *h.BubbleID)
		}
		sig.HeaderCount = len(ids)
		if len(ids) > 0 {
			sig.LastBubbleID = ids[len(ids)-1]
		}
		return sig, ids, nil
	}
	if raw, ok := present("conversation"); ok {
		var inline []header
		if json.Unmarshal(raw, &inline) != nil {
			return Signature{}, nil, NotChecked(UnknownFormat)
		}
		sig.HeaderCount = len(inline)
		if n := len(inline); n > 0 && inline[n-1].BubbleID != nil {
			sig.LastBubbleID = *inline[n-1].BubbleID
		}
	}
	return sig, nil, nil
}
