package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// filterTranscript filters reg's transcript with adapter, falling back to
// CursorAdapter's text format when Cursor's own JSONL filter reports the
// content isn't recognized as JSONL at all: the spec notes Cursor's
// hook-provided transcript path can point to either format depending on
// version, and the collector has no other way to tell which one it has
// until it tries. Any other adapter, or any other kind of filter failure,
// is returned as-is with no retry. A transcript whose filtered records come
// to more than maxBytes, or whose raw file is more than maxRawBytes(maxBytes),
// yields an error wrapping errTranscriptTooLarge.
var errTranscriptTooLarge = errors.New("transcript exceeds collection limit")

// errRecordTooLarge means one record of the transcript is longer than
// recordLimit.
var errRecordTooLarge = transcriptio.ErrRecordTooLarge

// checkFilteredSize is errTranscriptTooLarge for filtered records over
// maxBytes: the size limit applies to what is kept, not to the tool output
// the filter drops.
func checkFilteredSize(filtered archive.FilteredTranscript, maxBytes int64) error {
	if size := int64(filtered.Boundary.RetainedBytes); size > maxBytes {
		return fmt.Errorf("%w of %d bytes after filtering (%d bytes)", errTranscriptTooLarge, maxBytes, size)
	}
	return nil
}

// transcriptFileInfo is the identity of the exact file bytes one scan read:
// its size and its modification time at nanosecond resolution. Second
// resolution would not be enough, because an application can rewrite a
// transcript in place within the same second; see unchangedSinceLastScan.
type transcriptFileInfo struct {
	Size  int64
	Mtime int64
}

func statTranscript(info os.FileInfo) transcriptFileInfo {
	return transcriptFileInfo{Size: info.Size(), Mtime: info.ModTime().UnixNano()}
}

func filterTranscript(ctx context.Context, adapter archive.Adapter, reg archive.SessionRegistration, maxBytes int64) (archive.FilteredTranscript, transcriptFileInfo, error) {
	snapshot, err := transcriptio.Open(transcriptio.OS{}, reg.TranscriptPath, transcriptio.OpenPolicy{})
	if err != nil {
		return archive.FilteredTranscript{}, transcriptFileInfo{}, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = snapshot.Close() }()
	return filterSnapshot(ctx, snapshot, adapter, reg, maxBytes)
}

func filterSnapshot(ctx context.Context, file *transcriptio.Snapshot, adapter archive.Adapter, reg archive.SessionRegistration, maxBytes int64) (archive.FilteredTranscript, transcriptFileInfo, error) {
	stamp := file.Stamp()
	stat := transcriptFileInfo{Size: stamp.Size, Mtime: stamp.ModifiedAt.UnixNano()}
	boundary := stamp.Size
	if raw := maxRawBytes(maxBytes); boundary > raw {
		return archive.FilteredTranscript{}, stat, fmt.Errorf("%w of %d bytes before filtering", errTranscriptTooLarge, raw)
	}
	if err := ctx.Err(); err != nil {
		return archive.FilteredTranscript{}, stat, err
	}
	jsonBoundary, err := completeJSONLBoundary(contextReaderAt{ctx: ctx, reader: file}, boundary, recordLimit)
	if errors.Is(err, errRecordTooLarge) {
		return archive.FilteredTranscript{}, stat, err
	}
	if err != nil {
		return archive.FilteredTranscript{}, stat, fmt.Errorf("find complete transcript boundary: %w", err)
	}
	limited := &recordLimitReader{r: io.LimitReader(file.Reader(ctx), jsonBoundary), limit: recordLimit}
	filtered, err := filterReader(adapter, reg, limited)
	if err == nil {
		if change := file.Check(); change != nil {
			return archive.FilteredTranscript{}, stat, change
		}
		return filtered, stat, checkFilteredSize(filtered, maxBytes)
	}
	if limited.exceeded || errors.Is(err, archive.ErrRecordTooLarge) {
		return archive.FilteredTranscript{}, stat, errRecordTooLarge
	}
	cursorAdapter, ok := adapter.(archive.CursorAdapter)
	if !ok || !errors.Is(err, archive.ErrUnsafeSourceFormat) {
		return archive.FilteredTranscript{}, stat, err
	}
	// A plain-text transcript is one unit, bounded like one record: over the
	// limit it is the same recorded gap, not a per-pass error.
	if boundary > recordLimit {
		return archive.FilteredTranscript{}, stat, errRecordTooLarge
	}
	filtered, err = cursorAdapter.FilterText(file.Reader(ctx), reg.SessionStartedAt)
	if errors.Is(err, archive.ErrRecordTooLarge) {
		return archive.FilteredTranscript{}, stat, errRecordTooLarge
	}
	if err != nil {
		return filtered, stat, err
	}
	if change := file.Check(); change != nil {
		return archive.FilteredTranscript{}, stat, change
	}
	return filtered, stat, checkFilteredSize(filtered, maxBytes)
}

type contextReaderAt struct {
	ctx    context.Context
	reader io.ReaderAt
}

func (r contextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.ReadAt(p, off)
}

// errNotRegularFile means a hook-supplied transcript path names something
// other than a regular file: a FIFO, a device, a socket, or a directory.
var errNotRegularFile = transcriptio.ErrNotRegularFile

// openRegularFile opens a hook-supplied path for reading only if it is a
// regular file (following symlinks, as the stat-based change check does).
// The path is hook input and may name anything; opening a FIFO would block
// the whole pass until a writer appeared, and reading a device could never
// end. It is checked before opening, so nothing else is opened, and the open
// itself is non-blocking and checked again, so a regular file swapped for a
// FIFO in between fails instead of blocking.
func openRegularFile(path string) (*os.File, error) { return transcriptio.OS{}.OpenRegularFile(path) }

const boundaryChunk = 64 * 1024

func completeJSONLBoundary(file io.ReaderAt, size, limit int64) (int64, error) {
	return transcriptio.CompleteJSONLBoundary(file, size, limit)
}

// recordLimitReader passes a transcript through unchanged while checking that
// no line is longer than limit. The collector sets the limit, not the adapter,
// so the policy (block the session with a capture gap) stays with the
// collector; exceeded tells the caller that a failed filter was this. It adds
// one byte scan over data the filter reads anyway, and no allocation.
type recordLimitReader struct {
	r        io.Reader
	limit    int64
	line     int64
	exceeded bool
}

func (l *recordLimitReader) Read(p []byte) (int, error) {
	if l.exceeded {
		return 0, errRecordTooLarge
	}
	n, err := l.r.Read(p)
	for data := p[:n]; len(data) > 0; {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			l.line += int64(len(data))
			break
		}
		if l.line+int64(i) > l.limit {
			l.exceeded = true
			return 0, errRecordTooLarge
		}
		l.line, data = 0, data[i+1:]
	}
	if l.line > l.limit {
		l.exceeded = true
		return 0, errRecordTooLarge
	}
	return n, err
}
