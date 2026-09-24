package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// legacyCompleteJSONLBoundary is completeJSONLBoundary as it was before the
// record size limit: one fixed 2 MB tail read, kept here verbatim (apart from
// taking an io.ReaderAt) so the chunked scan can be checked against it.
func legacyCompleteJSONLBoundary(file io.ReaderAt, boundary int64) (int64, error) {
	if boundary == 0 {
		return 0, nil
	}
	const maxRecordBytes int64 = 2 * 1024 * 1024
	start := boundary - min(boundary, maxRecordBytes+1)
	tail := make([]byte, boundary-start)
	if _, err := file.ReadAt(tail, start); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if tail[len(tail)-1] == '\n' || json.Valid(bytes.TrimSpace(tail[bytes.LastIndexByte(tail, '\n')+1:])) {
		return boundary, nil
	}
	if lastNewline := bytes.LastIndexByte(tail, '\n'); lastNewline >= 0 {
		return start + int64(lastNewline) + 1, nil
	}
	return boundary, nil
}

// randomTranscriptTail builds a transcript-shaped byte string from complete
// records, partial records, long lines, CRLF and LF terminators, and blank
// or whitespace lines, so that the last newline lands anywhere from the last
// byte to several chunks back, or nowhere at all.
func randomTranscriptTail(rng *rand.Rand) []byte {
	var buf bytes.Buffer
	for pieces := rng.Intn(12); pieces >= 0; pieces-- {
		switch rng.Intn(9) {
		case 0:
			buf.WriteString(`{"type":"user","uuid":"u","message":{"content":"ok"}}`)
		case 1:
			buf.WriteString(`{"type":"user","uuid":"u","message":{"content":"`)
			buf.WriteString(strings.Repeat("x", rng.Intn(3*boundaryChunk)))
			buf.WriteString(`"}}`)
		case 2:
			buf.WriteString(`{"type":"user","uuid":"u","message":{"content":"`)
			buf.WriteString(strings.Repeat("x", rng.Intn(3*boundaryChunk)))
		case 3:
			buf.WriteString(`{"partial":`)
		case 4:
			buf.WriteByte('\n')
		case 5:
			buf.WriteString("\r\n")
		case 6:
			buf.WriteString("  \t ")
		case 7:
			buf.WriteByte('\r')
		case 8:
			buf.WriteString("\n\n")
		}
	}
	return buf.Bytes()
}

// The chunked scan answers exactly what the old fixed-tail read did on every
// input the old read covered: any file that fits inside its 2 MB tail. With a
// lowered limit the two still agree whenever the bytes after the last newline
// fit the limit, and beyond it the chunked scan is the one that reports the
// trailing record as too large instead of guessing.
func TestCompleteJSONLBoundaryMatchesTheFixedTailRead(t *testing.T) {
	const seed = 20260922
	rng := rand.New(rand.NewSource(seed))
	const lowered = int64(boundaryChunk + boundaryChunk/2)
	var (
		agreed, tooLarge, multiChunk int
	)
	for i := range 400 {
		content := randomTranscriptTail(rng)
		size := int64(len(content))
		if size > 2<<20 {
			// Outside the old read's cover; not a comparison.
			continue
		}
		want, err := legacyCompleteJSONLBoundary(bytes.NewReader(content), size)
		if err != nil {
			t.Fatalf("seed %d case %d: legacy: %v", seed, i, err)
		}
		got, err := completeJSONLBoundary(bytes.NewReader(content), size, archive.MaxRecordBytes)
		if err != nil || got != want {
			t.Fatalf("seed %d case %d (%d bytes): boundary %d err %v, legacy %d", seed, i, size, got, err, want)
		}
		agreed++
		trailing := size - int64(bytes.LastIndexByte(content, '\n')) - 1
		if trailing > boundaryChunk {
			multiChunk++
		}
		got, err = completeJSONLBoundary(bytes.NewReader(content), size, lowered)
		switch {
		case trailing <= lowered:
			if err != nil || got != want {
				t.Fatalf("seed %d case %d (%d bytes, %d trailing): lowered limit gave %d err %v, legacy %d", seed, i, size, trailing, got, err, want)
			}
		default:
			if !errors.Is(err, errRecordTooLarge) {
				t.Fatalf("seed %d case %d (%d bytes, %d trailing): lowered limit gave %d err %v, want errRecordTooLarge", seed, i, size, trailing, got, err)
			}
			tooLarge++
		}
	}
	if agreed < 300 || multiChunk < 20 || tooLarge < 20 {
		t.Fatalf("the generator did not cover the cases: %d compared, %d with a multi-chunk tail, %d over the lowered limit", agreed, multiChunk, tooLarge)
	}
}
