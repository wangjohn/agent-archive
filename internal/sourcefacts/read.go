package sourcefacts

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const (
	// HeaderBytes caps all preauthorization reading for one source.
	HeaderBytes = 256 << 10
	// HeaderRecords bounds leading records inspected for native task evidence.
	HeaderRecords = 64
)

// Header is a content-free, bounded observation of one Codex source.
type Header struct {
	// Identity survives understood history-pending outcomes as lookup evidence only.
	Identity             *codexmeta.CodexIdentity
	NativeCreatedAt      time.Time
	Meta                 CodexMeta
	Started              time.Time
	FirstTaskAt          time.Time
	Profile              CodexProfile
	Outcome              string
	Bytes                int64
	NativeReadBytes      int64
	NativeReadOperations int64
}

// OpenRegular opens within an approved root without blocking on FIFOs or
// permitting a symlink escape. The caller owns the returned file.
func OpenRegular(root, path string) (*os.File, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, errors.New("invalid source locator")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("source root unavailable")
	}
	defer func() { _ = r.Close() }()
	info, err := r.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	f, err := r.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("source unavailable")
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, errors.New("source changed while opening")
	}
	return f, nil
}

// ReadHeader opens a bounded source safely. Paths and native IDs are never
// included in its outcome codes or errors.
func ReadHeader(ctx context.Context, root, path string) Header {
	snapshot, err := transcriptio.Open(RootOpener{Root: root}, path, transcriptio.OpenPolicy{Root: root, RejectSymlinks: true})
	if err != nil {
		return Header{Outcome: "source_unavailable"}
	}
	defer func() { _ = snapshot.Close() }()
	reader := &measuredHeaderReader{reader: snapshot.Reader(ctx)}
	h := ReadCodexHeader(reader, path)
	h.NativeReadBytes, h.NativeReadOperations = reader.bytes, reader.operations
	if snapshot.Check() != nil || ctx.Err() != nil {
		return Header{Outcome: "source_changed", Bytes: h.Bytes, NativeReadBytes: h.NativeReadBytes, NativeReadOperations: h.NativeReadOperations}
	}
	return h
}

type measuredHeaderReader struct {
	reader            io.Reader
	bytes, operations int64
}

func (r *measuredHeaderReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.bytes += int64(n)
	r.operations++
	return n, err
}

// ReadCodexHeader inspects metadata and the FIRST task event, stopping before
// full transcript parsing. It does not retain prompt or response content.
func ReadCodexHeader(reader io.Reader, path string) (h Header) {
	h = Header{Outcome: "incomplete_metadata"}
	// Rejected or partial records need only retry bookkeeping. Never persist
	// unsupported, potentially enormous metadata strings or raw values.
	defer func() {
		if h.Outcome != "native_format" {
			h.Meta = CodexMeta{}
			h.Started = time.Time{}
			h.FirstTaskAt = time.Time{}
			h.Profile = ""
		}
	}()
	r := bufio.NewReaderSize(io.LimitReader(reader, HeaderBytes), 32<<10)
	for i := range HeaderRecords {
		line, err := r.ReadBytes('\n')
		h.Bytes += int64(len(line))
		if len(line) > 64<<10 {
			h.Outcome = "oversized_metadata"
			return h
		}
		if err != nil {
			return h
		}
		line = bytes.TrimSpace(line)
		if !json.Valid(line) {
			h.Outcome = "invalid_metadata"
			return h
		}
		if i == 0 {
			meta, start, found, e := ParseCodexMeta(line)
			if !found || e != nil {
				h.Outcome = "invalid_metadata"
				return h
			}
			meta.Timestamp = start.Format(time.RFC3339Nano)
			h.Meta, h.Started = safeMeta(meta), start
			if len(meta.ID) > 128 || len(meta.Originator) > 256 || len(meta.Version) > 128 {
				h.Meta = CodexMeta{}
				h.Outcome = "oversized_metadata"
				return h
			}
			identity, identityOutcome := meta.Identity(path)
			captureOutcome := meta.CaptureOutcome(path)
			knownHistory := captureOutcome == codexmeta.NativeFormat || captureOutcome == codexmeta.ChildHistoryPending || captureOutcome == codexmeta.ForkHistoryPending || captureOutcome == codexmeta.RelatedHistoryPending
			if identityOutcome == "" && knownHistory && !start.IsZero() {
				h.Identity = &identity
				h.NativeCreatedAt = start
			}
			if outcome := meta.CaptureOutcome(path); outcome != "native_format" {
				h.Outcome = string(outcome)
				return h
			}
			continue
		}
		if seen, native := NativeFirstTask(line); seen {
			h.FirstTaskAt = FirstTaskAt(line)
			if !native || h.FirstTaskAt.Before(h.Started.Add(-time.Second)) {
				h.Outcome = "inherited_history"
			} else {
				h.Outcome = "native_format"
				h.Profile = CodexFormatProfile(h.Meta)
			}
			return h
		}
	}
	return h
}

func safeMeta(m CodexMeta) CodexMeta {
	var source string
	if json.Unmarshal(m.Source, &source) == nil && m.LocalExecutionSource() {
		m.Source, _ = json.Marshal(source)
	} else {
		m.Source = nil
	}
	flag := func(raw json.RawMessage) json.RawMessage {
		if present(raw) {
			return json.RawMessage("true")
		}
		return nil
	}
	m.ForkedFrom = flag(m.ForkedFrom)
	m.ForkOrdinal = flag(m.ForkOrdinal)
	m.Parent = flag(m.Parent)
	m.HistoryBase = flag(m.HistoryBase)
	m.SubagentOrdinal = flag(m.SubagentOrdinal)
	return m
}
