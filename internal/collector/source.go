package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/state"
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
func (s sourceState) matches(signature state.ScanSignature) bool {
	if signature.SourceKind != s.kind {
		return false
	}
	if s.kind == archive.SourceKindCursorSQLite {
		return signature.CursorSignature() == s.cursor
	}
	return s.file == transcriptFileInfo{Size: signature.TranscriptSize, Mtime: signature.TranscriptMtime}
}

// sourceReader reads one registration's evidence from wherever its
// SourceKind says it lives.
type sourceReader interface {
	// Signature observes the source's current state without reading its
	// content, for unchangedSinceLastScan. An error means it can't be
	// observed, which is never "unchanged" except for a source recorded as
	// missing that still is: that error wraps os.ErrNotExist.
	Signature(ctx context.Context) (observed sourceState, err error)
	// Filter reads the whole source and filters it with adapter, returning
	// the state of exactly what it read. A source over the size limits (see
	// Options.MaxTranscriptBytes) is errTranscriptTooLarge, one record over
	// recordLimit errRecordTooLarge, and a missing source wraps
	// os.ErrNotExist.
	Filter(ctx context.Context, adapter archive.Adapter, maxBytes int64) (archive.FilteredTranscript, sourceState, error)
}

// newSourceReader returns reg's reader, or ok=false when there is nothing to
// read yet: a file session whose transcript path a hook has not reported.
func newSourceReader(reg archive.SessionRegistration, opts Options) (sourceReader, bool) {
	switch reg.SourceKind {
	case archive.SourceKindCursorSQLite:
		return cursorSQLiteReader{reg: reg, dbPath: opts.cursorDatabase(), pass: opts.cursorPass}, true
	case archive.SourceKindFile:
		fallthrough
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

func (r fileReader) Signature(context.Context) (sourceState, error) {
	info, err := os.Stat(r.reg.TranscriptPath)
	if err != nil {
		return sourceState{}, err
	}
	if !info.Mode().IsRegular() {
		return sourceState{}, errNotRegularFile
	}
	return sourceState{file: statTranscript(info)}, nil
}

func (r fileReader) Filter(_ context.Context, adapter archive.Adapter, maxBytes int64) (archive.FilteredTranscript, sourceState, error) {
	filtered, stat, err := filterTranscript(adapter, r.reg, maxBytes)
	return filtered, sourceState{file: stat}, err
}

// cursorSQLiteReader reads one chat, reg.SourceKey, from Cursor's database.
type cursorSQLiteReader struct {
	reg    archive.SessionRegistration
	dbPath string
	// pass is the collector pass's Reader, which takes at most one snapshot
	// of the database for all the chats the pass reads. Nil outside a pass.
	pass *cursorstore.Reader
}

// Signature reads only the chat's own rows, in place: the database file
// changes constantly, so its stat says nothing about one chat (spec, phase 2
// decision 7), and a snapshot of the whole database would cost every pass a
// copy of it.
func (r cursorSQLiteReader) Signature(ctx context.Context) (sourceState, error) {
	sig, err := cursorstore.ReadSignature(ctx, r.dbPath, r.reg.SourceKey)
	if err != nil {
		return sourceState{}, err
	}
	return sourceState{kind: archive.SourceKindCursorSQLite, cursor: sig}, nil
}

// filterCursorComposer filters one chat read from Cursor's database with the
// Cursor adapter's composer filter, after the size limits a transcript file
// gets: the chat's rows together are its size, and each row is one record.
func filterCursorComposer(adapter archive.Adapter, c cursorstore.Composer, maxBytes int64) (archive.FilteredTranscript, error) {
	cursor, ok := adapter.(archive.CursorAdapter)
	if !ok {
		return archive.FilteredTranscript{}, fmt.Errorf("a Cursor database chat can't be read by the %s adapter", adapter.Name())
	}
	if err := checkCursorChatSize(c, maxBytes); err != nil {
		return archive.FilteredTranscript{}, err
	}
	bubbles := make([]archive.CursorBubble, len(c.Bubbles))
	for i, b := range c.Bubbles {
		bubbles[i] = archive.CursorBubble{ID: b.ID, Value: b.Value}
	}
	filtered, err := cursor.FilterComposer(archive.CursorComposer{Composer: c.Composer, Bubbles: bubbles})
	if errors.Is(err, archive.ErrRecordTooLarge) {
		return archive.FilteredTranscript{}, errRecordTooLarge
	}
	if err != nil {
		return filtered, err
	}
	return filtered, checkFilteredSize(filtered, maxBytes)
}

// CursorChatSize is the size of a chat as read from Cursor's database: its
// composerData value and its message rows.
func CursorChatSize(c cursorstore.Composer) int64 {
	size := int64(len(c.Composer))
	for _, b := range c.Bubbles {
		size += int64(len(b.Value))
	}
	return size
}

// checkCursorChatSize is errRecordTooLarge for a row over recordLimit and
// errTranscriptTooLarge for a chat over maxRawBytes(maxBytes) before filtering.
func checkCursorChatSize(c cursorstore.Composer, maxBytes int64) error {
	if int64(len(c.Composer)) > recordLimit {
		return errRecordTooLarge
	}
	for _, b := range c.Bubbles {
		if int64(len(b.Value)) > recordLimit {
			return errRecordTooLarge
		}
	}
	if raw := maxRawBytes(maxBytes); CursorChatSize(c) > raw {
		return fmt.Errorf("%w of %d bytes before filtering", errTranscriptTooLarge, raw)
	}
	return nil
}

// FilterCursorChat filters one chat read from Cursor's database as a
// collector pass does, for a chat with no registration yet. A chat over the
// size limits is an error wrapping archive.ErrRecordTooLarge. Nothing is
// registered, written, or uploaded.
func FilterCursorChat(c cursorstore.Composer) (archive.FilteredTranscript, error) {
	filtered, err := filterCursorComposer(archive.CursorAdapter{}, c, DefaultMaxTranscriptBytes)
	if errors.Is(err, errRecordTooLarge) || errors.Is(err, errTranscriptTooLarge) {
		return archive.FilteredTranscript{}, fmt.Errorf("%w: %w", archive.ErrRecordTooLarge, err)
	}
	return filtered, err
}

// Filter first reads the chat's signature in place, so a chat Cursor deleted
// (or a composerData value that doesn't decode) costs no copy of the
// database, then reads the chat through the pass's Reader.
func (r cursorSQLiteReader) Filter(ctx context.Context, adapter archive.Adapter, maxBytes int64) (archive.FilteredTranscript, sourceState, error) {
	if _, err := cursorstore.ReadSignature(ctx, r.dbPath, r.reg.SourceKey); err != nil {
		// A missing database or chat wraps os.ErrNotExist, so the session
		// records a missing-source gap, as a deleted transcript does.
		return archive.FilteredTranscript{}, sourceState{}, fmt.Errorf("read Cursor chat: %w", err)
	}
	reader := r.pass
	if reader == nil {
		reader = cursorstore.NewReader(r.dbPath)
		defer func() { _ = reader.Close() }()
	}
	c, sig, err := reader.ReadComposer(ctx, r.reg.SourceKey)
	if errors.Is(err, cursorstore.ErrComposerNotFound) {
		// The chat is in the database now but not in the pass's snapshot,
		// taken earlier in the pass: Cursor wrote it since. That is not a
		// deleted chat, so no missing-source gap; the next pass reads it.
		return archive.FilteredTranscript{}, sourceState{}, fmt.Errorf("read Cursor chat: %w", cursorstore.NotChecked(cursorstore.ChangedDuringRead))
	}
	if err != nil {
		return archive.FilteredTranscript{}, sourceState{}, fmt.Errorf("read Cursor chat: %w", err)
	}
	observed := sourceState{kind: archive.SourceKindCursorSQLite, cursor: sig}
	filtered, err := filterCursorComposer(adapter, c, maxBytes)
	return filtered, observed, err
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

// afterCursorPass, when set by a test, runs as a pass ends with how many
// snapshots of Cursor's database the pass took.
var afterCursorPass func(snapshots int)

// openCursorPass gives the pass one Reader for Cursor's database when any
// session is read from it (Run has already swept snapshots a killed pass
// left behind).
// The returned function removes the pass's snapshot, and says if it could
// not: a copy of every Cursor chat left in the temporary directory is worth
// a failed pass (the next sweep removes it once it is stale).
func openCursorPass(registrations []archive.SessionRegistration, opts *Options) func() error {
	for _, reg := range registrations {
		if reg.SourceKind == archive.SourceKindCursorSQLite {
			reader := cursorstore.NewReader(opts.cursorDatabase())
			opts.cursorPass = reader
			return func() error {
				err := reader.Close()
				if afterCursorPass != nil {
					afterCursorPass(reader.Snapshots())
				}
				return err
			}
		}
	}
	return func() error { return nil }
}

// rememberFailedRead records, for a cursor-sqlite session whose chat was
// read but could not be captured for a reason that reading it again cannot
// change (a filter error, or a size limit), the state it failed at, with
// the error's text ("" for a recorded gap). unchangedSinceLastScan then
// skips the chat until its signature, or a derivation version, changes, so
// the failure costs one in-place signature read per pass rather than a copy
// of the database. A failure to read at all (a lock, a changed file) is
// left to be retried. The size limits in force are recorded too, so raising
// one reads the chat again, and so is the gap (blocked) a size limit
// recorded, for status.
func rememberFailedRead(local *state.Store, reg archive.SessionRegistration, adapter archive.Adapter, observed sourceState, opts Options, failure error, blocked state.BlockedReason) error {
	if observed.kind != archive.SourceKindCursorSQLite {
		return nil
	}
	message := ""
	if failure != nil {
		message = failure.Error()
	}
	return local.SaveScanSignature(reg.ArchiveSessionID, state.ScanSignature{
		ParserVersion: opts.parserVersion(), FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(),
		SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
		Failed: true, FailedError: message, FailedMaxBytes: opts.maxTranscriptBytes(), FailedRecordLimit: recordLimit,
		Blocked: blocked,
	})
}

// CaptureGapCursorChatRewritten marks a Cursor database chat whose messages
// changed after an earlier snapshot of it was taken, so a later snapshot
// replaced one that did not lead to it (see sessionScan.guard). A chat has at
// most one: its detail counts the rewrites, and its observation time is the
// last one's.
const CaptureGapCursorChatRewritten = "cursor_chat_rewritten"

// cursorRewriteProvenance is the rewrite gap's provenance: the collector's
// own, never a hook's, so it is not taken for a resume.
const cursorRewriteProvenance = "collector:cursor-rewrite"

// cursorRewriteDetail is the rewrite gap's detail for n rewrites. The
// earlier snapshot may have been published, or only held back (rate
// limited or declined), so the wording claims neither.
const cursorRewriteDetail = "Cursor changed messages of this chat after an earlier snapshot of it %d time(s); the snapshot is the chat as it was last read"

// withCursorRewriteGap returns evidence with the chat's one rewrite gap
// counting one more rewrite, observed at: an earlier rewrite gap is replaced,
// not added to, so a chat Cursor rewrites often (late token counts are
// routine) does not grow a gap per rewrite.
func withCursorRewriteGap(evidence []archive.SupplementalEvidence, at time.Time) []archive.SupplementalEvidence {
	rewrites := 0
	out := make([]archive.SupplementalEvidence, 0, len(evidence)+1)
	for _, e := range evidence {
		if e.Kind == archive.EvidenceKindCaptureGap && e.Provenance == cursorRewriteProvenance {
			n := 0
			detail, _ := e.Payload["detail"].(string)
			if _, err := fmt.Sscanf(detail, cursorRewriteDetail, &n); err != nil || n < 1 {
				n = 1
			}
			rewrites += n
			continue
		}
		out = append(out, e)
	}
	return append(out, archive.SupplementalEvidence{
		Kind: archive.EvidenceKindCaptureGap, ObservedAt: at.UTC(), Provenance: cursorRewriteProvenance,
		Payload: map[string]any{
			"code":   CaptureGapCursorChatRewritten,
			"detail": fmt.Sprintf(cursorRewriteDetail, rewrites+1),
		},
	})
}

// unchangedSinceFailureError reports a remembered failure again on a pass that
// skipped the chat because it has not changed since.
type unchangedSinceFailureError struct{ message string }

func (e unchangedSinceFailureError) Error() string {
	return e.message + " (the Cursor chat has not changed since)"
}
