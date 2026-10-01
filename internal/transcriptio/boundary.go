package transcriptio

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ErrRecordTooLarge means a record exceeds the caller's byte limit.
var ErrRecordTooLarge = errors.New("transcript record exceeds the record size limit")

// boundaryChunk is how much of the transcript CompleteJSONLBoundary reads at a
// time while it looks backward for the last newline.
const boundaryChunk = 64 * 1024

// CompleteJSONLBoundary returns the offset just past the transcript's last
// complete record, so a final record the harness is still writing is ignored.
// size was fixed by the caller before this check.
//
// A transcript that ends in a newline, the ordinary case, costs a one-byte
// read. Otherwise it scans backward from the end in boundaryChunk pieces for
// the last newline, never further back than limit+1 bytes: a trailing record
// that long cannot be read, and ErrRecordTooLarge says so. The trailing bytes
// after that newline are a complete record when they are valid JSON (a final
// record written without its newline); otherwise they are still being written
// and the boundary is the newline. A transcript with no newline at all is
// taken whole, as before.
func CompleteJSONLBoundary(file io.ReaderAt, size, limit int64) (int64, error) {
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
			return 0, ErrRecordTooLarge
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
