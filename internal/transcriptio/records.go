package transcriptio

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
)

// RecordWindow reports complete-record coverage without retaining raw text.
type RecordWindow struct {
	Bytes    int64
	Records  int
	Skipped  int
	Complete bool
}

// Records visits newline-terminated JSONL records in a bounded head or tail.
// The tail's leading partial record and the unfinished final record are skipped.
// A record larger than recordBytes is skipped with bounded memory.
func (s *Snapshot) Records(ctx context.Context, tail bool, windowBytes, recordBytes int64, visit func([]byte) bool) (RecordWindow, error) {
	var result RecordWindow
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if windowBytes <= 0 || recordBytes <= 0 {
		return result, errors.New("positive record window limits are required")
	}
	start := int64(0)
	end := min(s.stamp.Size, windowBytes)
	if tail {
		end = s.stamp.Size
		start = max(0, end-windowBytes)
	}
	result.Complete = start == 0 && end == s.stamp.Size
	counted := &windowReader{r: io.NewSectionReader(s, start, end-start)}
	reader := bufio.NewReaderSize(counted, int(min(end-start, 32*1024)))
	var consumed, probe int64
	partial := false
	if start > 0 {
		var previous [1]byte
		if _, err := s.ReadAt(previous[:], start-1); err != nil {
			return result, err
		}
		probe = 1
		result.Bytes = probe
		partial = previous[0] != '\n'
	}
	line := make([]byte, 0, min(recordBytes, end-start, 32*1024))
	large := false
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		chunk, err := reader.ReadSlice('\n')
		result.Bytes = counted.bytes + probe
		consumed += int64(len(chunk))
		if !large {
			if int64(len(line)+len(chunk)) > recordBytes {
				large = true
				line = line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(chunk) > 0 || len(line) > 0 || large {
			if partial || large || (len(chunk) > 0 && chunk[len(chunk)-1] != '\n') {
				result.Skipped++
				result.Complete = false
			} else if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				result.Records++
				if !visit(trimmed) {
					result.Complete = result.Complete && start+consumed >= end
					return result, s.Check()
				}
			}
		}
		line = line[:0]
		partial = false
		large = false
		if errors.Is(err, io.EOF) {
			return result, s.Check()
		}
		if err != nil {
			return result, err
		}
	}
}

// windowReader counts underlying reads, including bufio read-ahead.
type windowReader struct {
	r     io.Reader
	bytes int64
}

func (r *windowReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.bytes += int64(n)
	return n, err
}
