package collector

import (
	"bytes"
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"io"
	"os"
)

var testSources = builtin.NewBuiltins()

func testAdapter(name string) (archive.Adapter, error) { return sourceAdapter(testSources, name) }

var subagentMetaReads = &claude.MetadataReads

func filterTranscript(ctx context.Context, adapter archive.Adapter, reg archive.SessionRegistration, maxBytes int64) (archive.FilteredTranscript, transcriptFileInfo, error) {
	reg.Harness.Name = adapter.Name()
	source, _ := newSourceReader(reg, Options{Sources: testSources})
	out, observed, err := source.Filter(ctx, adapter, maxBytes)
	return out, observed.file, err
}

type fileReader struct{ reg archive.SessionRegistration }

func (r fileReader) Filter(ctx context.Context, a archive.Adapter, maxBytes int64) (archive.FilteredTranscript, sourceState, error) {
	out, stat, err := filterTranscript(ctx, a, r.reg, maxBytes)
	return out, sourceState{file: stat}, err
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
