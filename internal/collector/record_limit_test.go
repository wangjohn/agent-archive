package collector

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

const claudePromptLine = `{"type":"user","uuid":"p1","sessionId":"native-claude","timestamp":"2026-09-22T12:00:00Z","message":{"role":"user","content":"Run the tests."}}`

// toolResultLine is a Claude Code tool-result record whose bulk (bulkBytes)
// is in toolUseResult, the field that made real records megabytes long and
// that the filter drops.
func toolResultLine(uuid string, bulkBytes int) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"native-claude","timestamp":"2026-09-22T12:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"short summary"}]},"toolUseResult":{"stdout":%q}}`, uuid, strings.Repeat("x", bulkBytes))
}

func withCollectorRecordLimit(t *testing.T, limit int64) {
	t.Helper()
	saved := recordLimit
	recordLimit = limit
	t.Cleanup(func() { recordLimit = saved })
}

// The session that was refused whole now publishes, and the megabytes of
// dropped tool output stay out of the published bundle.
func TestFiveMegabyteToolResultRecordPublishes(t *testing.T) {
	local := newTestStore(t)
	remote := storage.NewMemoryStore()
	claudeSession(t, local, claudePromptLine+"\n"+toolResultLine("r1", 5<<20)+"\n")
	result := runAt(t, local, remote, time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC))
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
	bundle, _, found, err := local.LoadLastPublished("session-1")
	if err != nil || !found || len(bundle.NativeRecords) != 2 {
		t.Fatalf("published bundle: found=%t records=%d err=%v", found, len(bundle.NativeRecords), err)
	}
	if _, kept := bundle.NativeRecords[1]["toolUseResult"]; kept {
		t.Fatal("toolUseResult was published")
	}
	message, _ := bundle.NativeRecords[1]["message"].(map[string]any)
	if message == nil {
		t.Fatalf("the allowed fields were not kept: %#v", bundle.NativeRecords[1])
	}
}

// With the defaults a record over the limit cannot occur, since a transcript
// over the same limit is refused whole first. With the limit lowered, one
// long record mid-file or at the end is a capture gap recorded once, not a
// failure every pass, and it clears when the transcript changes.
func TestRecordOverTheLimitBlocksOnceAndClearsWhenTheFileChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"mid-file", claudePromptLine + "\n" + toolResultLine("r1", 8192) + "\n" + claudePromptLine + "\n"},
		{"trailing without newline", claudePromptLine + "\n" + toolResultLine("r1", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCollectorRecordLimit(t, 4096)
			local := newTestStore(t)
			remote := storage.NewMemoryStore()
			path := claudeSession(t, local, tc.content)
			at := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
			if err := local.SaveRequest("session-1", "stop", at); err != nil {
				t.Fatal(err)
			}
			result := runAt(t, local, remote, at)
			if len(result.Errors) != 0 || len(result.Published) != 0 {
				t.Fatalf("an oversize record must be a gap, not an error: %#v", result)
			}
			if reason, blocked, _ := local.LoadBlocked("session-1"); !blocked || reason != state.BlockedReasonRecordTooLarge {
				t.Fatalf("reason=%q blocked=%t", reason, blocked)
			}
			if string(state.BlockedReasonRecordTooLarge) != "record_size_limit" {
				t.Fatalf("gap code = %q", state.BlockedReasonRecordTooLarge)
			}
			status, _ := local.LoadStatus()
			if status.LastError != "" {
				t.Fatalf("status reports an error: %q", status.LastError)
			}
			cache := publishedPath(local, "session-1")
			before := mtime(t, cache)
			if result := runAt(t, local, remote, at.Add(time.Minute)); len(result.Errors) != 0 {
				t.Fatalf("second pass: %#v", result)
			}
			if !mtime(t, cache).Equal(before) {
				t.Fatal("the gap was rewritten on the second pass")
			}

			writeTranscript(t, dirOf(path), baseOf(path), claudePromptLine+"\n"+toolResultLine("r1", 1024)+"\n")
			result = runAt(t, local, remote, at.Add(2*time.Minute))
			if len(result.Published) != 1 || len(result.Errors) != 0 {
				t.Fatalf("the changed transcript did not publish: %#v", result)
			}
			if _, blocked, _ := local.LoadBlocked("session-1"); blocked {
				t.Fatal("the gap outlived the oversize record")
			}
		})
	}
}

// countingReaderAt records how many bytes completeJSONLBoundary reads.
type countingReaderAt struct {
	r    io.ReaderAt
	read int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

func TestCompleteJSONLBoundaryScansBackwardInChunks(t *testing.T) {
	record := func(bulk int) string { return toolResultLine("r", bulk) }
	partial := func(bulk int) string { s := record(bulk); return s[:len(s)-5] } // no closing, invalid JSON
	for _, tc := range []struct {
		name    string
		content string
		limit   int64
		want    func(content string) int64
		wantErr error
	}{
		{"ends with a newline", record(10) + "\n" + record(10) + "\n", 1 << 20, func(c string) int64 { return int64(len(c)) }, nil},
		{"partial final record longer than a chunk", record(10) + "\n" + partial(3*boundaryChunk), 1 << 20,
			func(c string) int64 { return int64(strings.LastIndexByte(c, '\n') + 1) }, nil},
		{"complete final record without newline, longer than a chunk", record(10) + "\n" + record(3*boundaryChunk), 1 << 20,
			func(c string) int64 { return int64(len(c)) }, nil},
		{"last newline several chunks back", record(10) + "\n" + partial(5*boundaryChunk+17), 1 << 20,
			func(c string) int64 { return int64(strings.LastIndexByte(c, '\n') + 1) }, nil},
		{"no newline at all", record(10), 1 << 20, func(c string) int64 { return int64(len(c)) }, nil},
		{"only one incomplete line, shorter than a chunk", partial(10), 1 << 20, func(c string) int64 { return int64(len(c)) }, nil},
		{"partial final record, file shorter than a chunk", record(10) + "\n" + partial(10), 1 << 20,
			func(c string) int64 { return int64(strings.LastIndexByte(c, '\n') + 1) }, nil},
		{"CRLF, ends with a newline", record(10) + "\r\n" + record(10) + "\r\n", 1 << 20, func(c string) int64 { return int64(len(c)) }, nil},
		{"CRLF, partial final record longer than a chunk", record(10) + "\r\n" + partial(3*boundaryChunk), 1 << 20,
			func(c string) int64 { return int64(strings.LastIndexByte(c, '\n') + 1) }, nil},
		{"CRLF, final record written up to its carriage return", record(10) + "\r\n" + record(10) + "\r", 1 << 20,
			func(c string) int64 { return int64(len(c)) }, nil},
		{"trailing record of exactly the limit", record(10) + "\n" + strings.Repeat("y", 200<<10), 200 << 10,
			func(c string) int64 { return int64(strings.LastIndexByte(c, '\n') + 1) }, nil},
		{"trailing record over the limit", record(10) + "\n" + partial(300<<10), 200 << 10, nil, errRecordTooLarge},
		{"trailing record one byte over the limit", record(10) + "\n" + strings.Repeat("y", 200<<10+1), 200 << 10, nil, errRecordTooLarge},
		{"single record over the limit", partial(300 << 10), 200 << 10, nil, errRecordTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := completeJSONLBoundary(strings.NewReader(tc.content), int64(len(tc.content)), tc.limit)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.want(tc.content); got != want {
				t.Fatalf("boundary = %d, want %d (length %d)", got, want, len(tc.content))
			}
		})
	}

	// The ordinary case reads one byte, whatever the file's size.
	content := strings.Repeat(record(1000)+"\n", 200)
	counter := &countingReaderAt{r: strings.NewReader(content)}
	if got, err := completeJSONLBoundary(counter, int64(len(content)), archive.MaxRecordBytes); err != nil || got != int64(len(content)) || counter.read != 1 {
		t.Fatalf("boundary=%d err=%v read %d bytes", got, err, counter.read)
	}
}

// The limiter passes the transcript through unchanged, across any read
// boundaries, and stops at the first line longer than the limit.
func TestRecordLimitReader(t *testing.T) {
	content := claudePromptLine + "\n" + toolResultLine("r1", 900) + "\n" + claudePromptLine
	passed, err := io.ReadAll(&recordLimitReader{r: iotest.OneByteReader(strings.NewReader(content)), limit: 2048})
	if err != nil || !bytes.Equal(passed, []byte(content)) {
		t.Fatalf("err=%v, content changed=%t", err, !bytes.Equal(passed, []byte(content)))
	}
	for _, reader := range []io.Reader{strings.NewReader(content), iotest.HalfReader(strings.NewReader(content))} {
		limited := &recordLimitReader{r: reader, limit: 512}
		if _, err := io.ReadAll(limited); !errors.Is(err, errRecordTooLarge) || !limited.exceeded {
			t.Fatalf("err=%v exceeded=%t", err, limited.exceeded)
		}
	}
}

func dirOf(path string) string { return path[:strings.LastIndexByte(path, '/')] }

func baseOf(path string) string { return path[strings.LastIndexByte(path, '/')+1:] }
