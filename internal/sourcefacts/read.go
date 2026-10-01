package sourcefacts

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// HeaderBytes caps all preauthorization reading for one source.
	HeaderBytes = 256 << 10
	// HeaderRecords bounds leading records inspected for native task evidence.
	HeaderRecords = 64
)

// Header is a content-free, bounded observation of one Codex source.
type Header struct {
	Meta    CodexMeta
	Started time.Time
	Outcome string
	Bytes   int64
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
func ReadHeader(root, path string) Header {
	f, err := OpenRegular(root, path)
	if err != nil {
		return Header{Outcome: "source_unavailable"}
	}
	defer func() { _ = f.Close() }()
	return ReadCodexHeader(f, path)
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
		}
	}()
	r := bufio.NewReaderSize(io.LimitReader(reader, HeaderBytes), 32<<10)
	for i := 0; i < HeaderRecords; i++ {
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
			nativeStart, e := time.Parse(time.RFC3339Nano, meta.Timestamp)
			if e != nil {
				h.Outcome = "invalid_metadata"
				return h
			}
			start = nativeStart.UTC()
			meta.Timestamp = start.Format(time.RFC3339Nano)
			h.Meta, h.Started = safeMeta(meta), start
			if len(meta.ID) > 128 || len(meta.Originator) > 256 || len(meta.Version) > 128 {
				h.Meta = CodexMeta{}
				h.Outcome = "oversized_metadata"
				return h
			}
			if !meta.ValidateIdentity(path) {
				h.Outcome = "invalid_identity"
				return h
			}
			if outcome := meta.Classification(); outcome != "native_format" {
				h.Outcome = outcome
				return h
			}
			continue
		}
		if seen, native := NativeFirstTask(line); seen {
			if !native || FirstTaskAt(line).Before(h.Started.Add(-time.Second)) {
				h.Outcome = "inherited_history"
			} else {
				h.Outcome = "native_format"
			}
			return h
		}
	}
	return h
}

func safeMeta(m CodexMeta) CodexMeta {
	var source string
	if json.Unmarshal(m.Source, &source) == nil && (source == "cli" || source == "vscode") {
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
