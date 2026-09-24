package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// sourceState identifies exactly what one read of a session's source saw,
// for the scan signature: a transcript file's size and modification time, or
// a Cursor chat's Signature.
type sourceState struct {
	kind   archive.SourceKind
	file   transcriptFileInfo
	cursor cursorstore.Signature
}

// empty reports a source that exists but holds nothing yet.
func (s sourceState) empty() bool {
	if s.kind == archive.SourceKindCursorSQLite {
		return s.cursor.HeaderCount == 0
	}
	return s.file.Size == 0
}

// matches reports whether signature was recorded for exactly this state.
func (s sourceState) matches(signature scanSignature) bool {
	if signature.SourceKind != s.kind {
		return false
	}
	if s.kind == archive.SourceKindCursorSQLite {
		return signature.cursorSignature() == s.cursor
	}
	return s.file == transcriptFileInfo{Size: signature.TranscriptSize, Mtime: signature.TranscriptMtime}
}

// sourceReader reads one registration's evidence from wherever its
// SourceKind says it lives.
type sourceReader interface {
	// Signature observes the source's current state without reading its
	// content, for unchangedSinceLastScan. ok is false when it can't be
	// observed (missing, unreadable), which is never "unchanged".
	Signature(ctx context.Context) (state sourceState, ok bool)
	// Filter reads the whole source and filters it with adapter, returning
	// the state of exactly what it read. A source over maxBytes is
	// errTranscriptTooLarge, one record over recordLimit errRecordTooLarge,
	// and a missing source wraps os.ErrNotExist.
	Filter(ctx context.Context, adapter archive.Adapter, maxBytes int64) (archive.FilteredTranscript, sourceState, error)
}

// newSourceReader returns reg's reader, or ok=false when there is nothing to
// read yet: a file session whose transcript path a hook has not reported.
func newSourceReader(local *LocalStore, reg archive.SessionRegistration, opts Options) (sourceReader, bool) {
	switch reg.SourceKind {
	case archive.SourceKindCursorSQLite:
		return cursorSQLiteReader{reg: reg, dbPath: opts.cursorDatabase(), scratchDir: local.cursorSnapshotDir()}, true
	default:
		if reg.TranscriptPath == "" {
			return nil, false
		}
		return fileReader{reg: reg}, true
	}
}

// fileReader reads a transcript file at reg.TranscriptPath.
type fileReader struct {
	reg archive.SessionRegistration
}

func (r fileReader) Signature(context.Context) (sourceState, bool) {
	info, err := os.Stat(r.reg.TranscriptPath)
	if err != nil || !info.Mode().IsRegular() {
		return sourceState{}, false
	}
	return sourceState{file: statTranscript(info)}, true
}

func (r fileReader) Filter(_ context.Context, adapter archive.Adapter, maxBytes int64) (archive.FilteredTranscript, sourceState, error) {
	filtered, stat, err := filterTranscript(adapter, r.reg, maxBytes)
	return filtered, sourceState{file: stat}, err
}

// cursorSQLiteReader reads one chat, reg.SourceKey, from Cursor's database.
type cursorSQLiteReader struct {
	reg        archive.SessionRegistration
	dbPath     string
	scratchDir string
}

// Signature reads only the chat's composerData row: the database file
// changes constantly, so its stat says nothing about one chat (spec, phase 2
// decision 7), and a snapshot of the whole database would cost every pass a
// copy of it.
func (r cursorSQLiteReader) Signature(ctx context.Context) (sourceState, bool) {
	sig, err := cursorstore.ReadSignature(ctx, r.dbPath, r.reg.SourceKey)
	if err != nil {
		return sourceState{}, false
	}
	return sourceState{kind: archive.SourceKindCursorSQLite, cursor: sig}, true
}

// errCursorSourceNotWired is what the cursor-sqlite reader's Filter returns
// until the composer adapter (archive.CursorAdapter.FilterComposer) is wired
// into filterCursorComposer. The session is left retryable, as any failed
// filter is.
var errCursorSourceNotWired = errors.New("reading Cursor database chats is not wired to the composer adapter yet")

// filterCursorComposer filters one chat read from Cursor's database.
//
// TODO(P3): return archive.CursorAdapter{}.FilterComposer(archive.CursorComposer(c))
// once P1's archive.CursorComposer exists; cursorstore.Composer has its
// shape field for field.
func filterCursorComposer(_ archive.Adapter, _ cursorstore.Composer) (archive.FilteredTranscript, error) {
	return archive.FilteredTranscript{}, errCursorSourceNotWired
}

func (r cursorSQLiteReader) Filter(ctx context.Context, adapter archive.Adapter, maxBytes int64) (archive.FilteredTranscript, sourceState, error) {
	c, sig, err := cursorstore.ReadComposer(ctx, r.dbPath, r.reg.SourceKey, r.scratchDir)
	if err != nil {
		// A missing database or chat wraps os.ErrNotExist, so the session
		// records a missing-source gap, as a deleted transcript does.
		return archive.FilteredTranscript{}, sourceState{}, fmt.Errorf("read Cursor chat: %w", err)
	}
	state := sourceState{kind: archive.SourceKindCursorSQLite, cursor: sig}
	size := int64(len(c.Composer))
	if int64(len(c.Composer)) > recordLimit {
		return archive.FilteredTranscript{}, state, errRecordTooLarge
	}
	for _, b := range c.Bubbles {
		if int64(len(b.Value)) > recordLimit {
			return archive.FilteredTranscript{}, state, errRecordTooLarge
		}
		size += int64(len(b.Value))
	}
	if size > maxBytes {
		return archive.FilteredTranscript{}, state, fmt.Errorf("%w of %d bytes", errTranscriptTooLarge, maxBytes)
	}
	filtered, err := filterCursorComposer(adapter, c)
	return filtered, state, err
}

// cursorDatabase is Cursor's state.vscdb for this user.
func (o Options) cursorDatabase() string {
	if o.CursorDatabase != "" {
		return o.CursorDatabase
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return cursorstore.StateDatabase(home)
}

// cursorSnapshotDir is where Cursor database snapshots are taken, inside
// the private archive home.
func (s *LocalStore) cursorSnapshotDir() string {
	return filepath.Join(s.home, "cursor-snapshots")
}
