package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
// all its messages (spec, phase 2 decision 7): the database file changes
// constantly, so its stat can't. It is comparable with ==.
type Signature struct {
	// LastUpdatedAt is composerData.lastUpdatedAt in Unix milliseconds, zero
	// when absent.
	LastUpdatedAt int64
	// HeaderCount is how many messages the chat lists.
	HeaderCount int
	// LastBubbleID is the last listed message's ID.
	LastBubbleID string
	// MessageRows is how many listed messages have a row (a key, whatever
	// its value), so a row that arrives after its header changes the
	// signature. Counting keys reads only the key index.
	MessageRows int
	// LastMessageHash is a hash of the last listed message's row, "" when it
	// has none, so an edit to the message being written changes it.
	LastMessageHash string
}

// ErrComposerNotFound means the database has no composerData row for the
// chat: Cursor deleted it. It wraps fs.ErrNotExist.
var ErrComposerNotFound = fmt.Errorf("no such Cursor chat: %w", fs.ErrNotExist)

// Reader reads chats from one Cursor database, taking at most one snapshot
// of it however many chats it reads: the first chat read while Cursor runs
// copies the database, and every later one reads that copy. A collector pass
// holds one Reader and closes it when the pass ends, which removes the copy.
// A Reader is not safe for concurrent use.
type Reader struct {
	dbPath string
	// snapDir is the private directory holding the copy, "" until one is
	// taken; copyPath is the copy, "" until it is complete.
	snapDir, copyPath string
	// lock holds the snapshot directory's lock while the copy is in use,
	// so no sweep removes it (see snapshotLockName).
	lock *os.File
	// snapErr is why the one snapshot attempt failed. It is not retried:
	// every later chat read through this Reader, for the rest of the pass,
	// fails with it without trying to copy the database again.
	snapErr   error
	snapshots int
	swept     bool
}

// NewReader returns a Reader for the database at dbPath. Nothing is opened
// until a chat is read.
func NewReader(dbPath string) *Reader { return &Reader{dbPath: dbPath} }

// Snapshots is how many copies of the database this Reader took: 0 or 1.
func (r *Reader) Snapshots() int { return r.snapshots }

// Close removes the Reader's snapshot, if it took one. It is safe to call
// more than once, and the Reader may be used again afterwards.
func (r *Reader) Close() error {
	dir, lock := r.snapDir, r.lock
	r.snapDir, r.copyPath, r.snapErr, r.lock = "", "", nil, nil
	if dir == "" {
		return nil
	}
	err := os.RemoveAll(dir)
	if lock != nil {
		// Released only after the copy is gone.
		lock.Close()
	}
	if err != nil {
		return errors.New("remove the Cursor database snapshot")
	}
	return nil
}

// ReadComposer reads one chat through a Reader of its own and removes any
// snapshot before returning; see Reader.ReadComposer.
func ReadComposer(ctx context.Context, dbPath, composerID string) (Composer, Signature, error) {
	r := NewReader(dbPath)
	c, sig, err := func() (Composer, Signature, error) {
		defer r.Close()
		return r.ReadComposer(ctx, composerID)
	}()
	return c, sig, err
}

// ReadComposer reads one chat consistently: its composerData row and its
// bubbleId:<composerID>:<bubbleID> rows, in header order.
//
// With Cursor running, the chat is read from the Reader's snapshot, taken
// on first use with SQLite's online backup API (spec, phase 2 decision 3):
// from a read-only connection opened as Read opens it, into a 0600 file in a
// new private directory under SnapshotRoot. The backup is one read
// transaction over the whole database, so the copy is one committed state
// even while Cursor writes; a lock held past the timeout fails the read
// rather than reading partially. With Cursor closed, the backup would
// create -wal beside the source, so the chat is read in place as Read does,
// immutable and checked afterwards, and nothing is copied.
//
// A missing database is ErrNoDatabase and a missing chat
// ErrComposerNotFound; both wrap fs.ErrNotExist. Anything else that can't be
// read safely is a *NotCheckedError.
func (r *Reader) ReadComposer(ctx context.Context, composerID string) (Composer, Signature, error) {
	if composerID == "" {
		return Composer{}, Signature{}, errors.New("a Cursor composer ID is required")
	}
	if !r.swept {
		r.swept = true
		RemoveStaleSnapshots()
	}
	var c Composer
	var sig Signature
	read := func(ctx context.Context, db *sql.DB) error {
		var err error
		c, sig, err = queryComposer(ctx, db, composerID)
		return err
	}
	var err error
	switch {
	case r.copyPath != "":
		err = r.readCopy(ctx, read)
	case r.snapErr != nil:
		err = r.snapErr
	default:
		var src source
		if src, err = resolve(r.dbPath); err != nil {
			break
		}
		if !src.live {
			err = readInPlace(ctx, src, Options{}, read)
			break
		}
		if err = r.snapshot(ctx, src); err != nil {
			r.snapErr = err
			break
		}
		err = r.readCopy(ctx, read)
	}
	if err != nil {
		return Composer{}, Signature{}, err
	}
	return c, sig, nil
}

// ReadSignature reads the chat's Signature in place, as Read does, inside
// one read transaction: the composerData row, which of its messages have
// rows, and the last message's row. It costs a few indexed rows, never a
// copy of the database, and agrees with what ReadComposer returns for the
// same state.
func ReadSignature(ctx context.Context, dbPath, composerID string) (Signature, error) {
	if composerID == "" {
		return Signature{}, errors.New("a Cursor composer ID is required")
	}
	var sig Signature
	err := Read(ctx, dbPath, Options{}, func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // a read-only transaction; nothing to undo
		value, err := composerRow(ctx, tx, composerID)
		if err != nil {
			return err
		}
		var ids []string
		if sig, ids, err = decodeHeaders(value); err != nil || len(ids) == 0 {
			return err
		}
		prefix := bubblePrefix(composerID)
		rows, err := tx.QueryContext(ctx, bubbleKeyQuery, prefix, bubbleUpper(prefix))
		if err != nil {
			return err
		}
		defer rows.Close()
		present := map[string]bool{}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			present[strings.TrimPrefix(key, prefix)] = true
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if present[id] {
				sig.MessageRows++
			}
		}
		var last []byte
		err = tx.QueryRowContext(ctx, `SELECT value FROM cursorDiskKV WHERE key = ?`, prefix+ids[len(ids)-1]).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		sig.LastMessageHash = messageHash(last)
		return nil
	})
	if err != nil {
		return Signature{}, err
	}
	return sig, nil
}

// readCopy reads the Reader's snapshot. The copy is this process's own and
// nothing writes it, so immutable is exact, and it opens no side file even
// though the copy's header still says WAL.
func (r *Reader) readCopy(ctx context.Context, read func(context.Context, *sql.DB) error) error {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	if err := query(ctx, dsn(r.copyPath, false), read); err != nil {
		return notChecked(err)
	}
	return nil
}

// afterSnapshot, when set by a test, runs once the copy is written and
// before it is read.
var afterSnapshot func(copyPath string)

// snapshot copies the live database src into a new private directory under
// SnapshotRoot. The directory is recorded before anything is written into
// it, so Close removes whatever a failure, or a panic, left.
func (r *Reader) snapshot(ctx context.Context, src source) error {
	root, err := SnapshotRoot()
	if err != nil {
		return err
	}
	// MkdirTemp creates the directory 0700.
	dir, err := os.MkdirTemp(root, snapshotPrefix)
	if err != nil {
		return errors.New("create a Cursor database snapshot directory")
	}
	r.snapDir = dir
	if r.lock, err = lockSnapshot(dir); err != nil {
		return errors.New("lock a Cursor database snapshot directory")
	}
	copyPath := filepath.Join(dir, "state.vscdb")
	// Created here, empty and 0600, so SQLite opens it rather than creating
	// it with the default mode.
	f, err := os.OpenFile(copyPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create a Cursor database snapshot file")
	}
	if err := f.Close(); err != nil {
		return errors.New("create a Cursor database snapshot file")
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	r.snapshots++
	if err := backup(ctx, dsn(src.path, true), copyPath); err != nil {
		r.Close()
		return notChecked(err)
	}
	r.copyPath = copyPath
	if afterSnapshot != nil {
		afterSnapshot(copyPath)
	}
	return nil
}

// backuper is the modernc.org/sqlite connection's online backup API.
type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

// backupRetry is how long backup waits before retrying a busy source.
const backupRetry = 25 * time.Millisecond

// backupRetried, when set by a test, runs before each busy retry.
var backupRetried func()

// errBackupIncomplete is a Step(-1) that reports pages left to copy, which
// it should never do.
var errBackupIncomplete = errors.New("the Cursor database backup stopped before the last page")

// backup copies the database srcDSN opens into the file at dst with SQLite's
// online backup API, in one Step(-1) so the copy is one read transaction's
// state. That step can't be interrupted; SQLITE_BUSY and SQLITE_LOCKED are
// retried until ctx ends.
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
			more, stepErr := bk.Step(-1)
			finishErr := bk.Finish()
			switch {
			case stepErr == nil && more:
				return errBackupIncomplete
			case stepErr == nil:
				return finishErr
			case !busy(stepErr):
				return stepErr
			}
			if backupRetried != nil {
				backupRetried()
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

// querier is a database or a transaction.
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// composerRow reads the chat's composerData value.
func composerRow(ctx context.Context, db querier, composerID string) ([]byte, error) {
	var value []byte
	err := db.QueryRowContext(ctx, `SELECT value FROM cursorDiskKV WHERE key = ?`, "composerData:"+composerID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && value == nil) {
		return nil, ErrComposerNotFound
	}
	return value, err
}

// bubbleQuery reads one chat's message rows through the key index, between
// the prefix bubbleId:<composerID>: and bubbleUpper of it, which bracket
// exactly the keys with the prefix. bubbleKeyQuery reads only their keys,
// from the key index alone (a covering index), so a signature costs no read
// of the rows themselves however long the chat.
const (
	bubbleQuery    = `SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ?`
	bubbleKeyQuery = `SELECT key FROM cursorDiskKV WHERE key >= ? AND key < ?`
)

func bubblePrefix(composerID string) string { return "bubbleId:" + composerID + ":" }

// bubbleUpper is prefix with its final ':' raised to ';'.
func bubbleUpper(prefix string) string { return prefix[:len(prefix)-1] + ";" }

// messageHash identifies a message row's value; "" for a missing row.
func messageHash(value []byte) string {
	if value == nil {
		return ""
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:16])
}

// queryComposer reads the chat's composerData row, then its message rows.
// It runs on a snapshot or an unchanged immutable file, so the two
// statements see the same state.
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
	prefix := bubblePrefix(composerID)
	rows, err := db.QueryContext(ctx, bubbleQuery, prefix, bubbleUpper(prefix))
	if err != nil {
		return Composer{}, Signature{}, err
	}
	defer rows.Close()
	found := map[string]json.RawMessage{}
	keys := map[string]bool{}
	for rows.Next() {
		var key string
		var v []byte
		if err := rows.Scan(&key, &v); err != nil {
			return Composer{}, Signature{}, err
		}
		id := strings.TrimPrefix(key, prefix)
		keys[id] = true
		if v != nil {
			found[id] = v
		}
	}
	if err := rows.Err(); err != nil {
		return Composer{}, Signature{}, err
	}
	for i, id := range ids {
		c.Bubbles[i] = Bubble{ID: id, Value: found[id]}
		if keys[id] {
			sig.MessageRows++
		}
	}
	sig.LastMessageHash = messageHash(c.Bubbles[len(ids)-1].Value)
	return c, sig, nil
}

// decodeHeaders reads the chat's Signature, apart from its message rows, and
// its message IDs in order from its composerData value:
// fullConversationHeadersOnly, or in older chats the inline conversation. An
// older chat's messages are inline, so it lists no message rows to read. A
// value that is not an object, or a message list or ID of another shape, is
// UnknownFormat.
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
