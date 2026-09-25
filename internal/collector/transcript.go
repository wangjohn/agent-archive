package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/archive"
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
var errRecordTooLarge = errors.New("transcript record exceeds the record size limit")

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

func filterTranscript(adapter archive.Adapter, reg archive.SessionRegistration, maxBytes int64) (archive.FilteredTranscript, transcriptFileInfo, error) {
	file, err := openRegularFile(reg.TranscriptPath)
	if err != nil {
		return archive.FilteredTranscript{}, transcriptFileInfo{}, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return archive.FilteredTranscript{}, transcriptFileInfo{}, fmt.Errorf("stat transcript: %w", err)
	}
	// Taken before any read, so it describes the bytes this scan is about to
	// consume. A write that lands afterwards moves the mtime and is therefore
	// seen as a change by the next pass, which is the conservative direction.
	stat := statTranscript(info)
	boundary := info.Size()
	if raw := maxRawBytes(maxBytes); boundary > raw {
		return archive.FilteredTranscript{}, stat, fmt.Errorf("%w of %d bytes before filtering", errTranscriptTooLarge, raw)
	}
	if boundary < 0 {
		return archive.FilteredTranscript{}, stat, errors.New("transcript has invalid size")
	}
	jsonBoundary, err := completeJSONLBoundary(file, boundary, recordLimit)
	if errors.Is(err, errRecordTooLarge) {
		return archive.FilteredTranscript{}, stat, err
	}
	if err != nil {
		return archive.FilteredTranscript{}, stat, fmt.Errorf("find complete transcript boundary: %w", err)
	}
	limited := &recordLimitReader{r: io.NewSectionReader(file, 0, jsonBoundary), limit: recordLimit}
	filtered, err := adapter.FilterJSONL(limited)
	if err == nil {
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
	filtered, err = cursorAdapter.FilterText(io.NewSectionReader(file, 0, boundary), reg.SessionStartedAt)
	if errors.Is(err, archive.ErrRecordTooLarge) {
		return archive.FilteredTranscript{}, stat, errRecordTooLarge
	}
	if err != nil {
		return filtered, stat, err
	}
	return filtered, stat, checkFilteredSize(filtered, maxBytes)
}

// errNotRegularFile means a hook-supplied transcript path names something
// other than a regular file: a FIFO, a device, a socket, or a directory.
var errNotRegularFile = errors.New("transcript is not a regular file")

// openRegularFile opens a hook-supplied path for reading only if it is a
// regular file (following symlinks, as the stat-based change check does).
// The path is hook input and may name anything; opening a FIFO would block
// the whole pass until a writer appeared, and reading a device could never
// end. It is checked before opening, so nothing else is opened, and the open
// itself is non-blocking and checked again, so a regular file swapped for a
// FIFO in between fails instead of blocking.
func openRegularFile(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w (%s)", errNotRegularFile, info.Mode().Type())
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if info, err = file.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err == nil {
			err = fmt.Errorf("%w (%s)", errNotRegularFile, info.Mode().Type())
		}
		return nil, err
	}
	return file, nil
}

// boundaryChunk is how much of the transcript completeJSONLBoundary reads at a
// time while it looks backward for the last newline.
const boundaryChunk = 64 * 1024

// completeJSONLBoundary returns the offset just past the transcript's last
// complete record, so a final record the harness is still writing is ignored.
// size was fixed by the caller before this check.
//
// A transcript that ends in a newline, the ordinary case, costs a one-byte
// read. Otherwise it scans backward from the end in boundaryChunk pieces for
// the last newline, never further back than limit+1 bytes: a trailing record
// that long cannot be read, and errRecordTooLarge says so. The trailing bytes
// after that newline are a complete record when they are valid JSON (a final
// record written without its newline); otherwise they are still being written
// and the boundary is the newline. A transcript with no newline at all is
// taken whole, as before.
func completeJSONLBoundary(file io.ReaderAt, size, limit int64) (int64, error) {
	if size <= 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	// The earliest offset a newline may have for the record after it to fit.
	floor := max(0, size-limit-1)
	newline := int64(-1)
	chunk := make([]byte, boundaryChunk)
	for end := size; end > floor && newline < 0; {
		start := max(floor, end-boundaryChunk)
		buf := chunk[:end-start]
		if _, err := file.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			newline = start + int64(i)
		}
		end = start
	}
	if newline < 0 {
		if size > limit {
			return 0, errRecordTooLarge
		}
		return size, nil
	}
	trailing := make([]byte, size-newline-1)
	if _, err := file.ReadAt(trailing, newline+1); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if json.Valid(bytes.TrimSpace(trailing)) {
		return size, nil
	}
	return newline + 1, nil
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
