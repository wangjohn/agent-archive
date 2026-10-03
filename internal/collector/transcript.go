package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
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

func filterSnapshot(ctx context.Context, file agentapi.FileInput, adapter archive.Adapter, reg archive.SessionRegistration, maxBytes int64) (archive.FilteredTranscript, transcriptFileInfo, error) {
	stamp := file.Stamp()
	stat := transcriptFileInfo{Size: stamp.Size, Mtime: stamp.ModifiedAt.UnixNano()}
	if stamp.Size > maxRawBytes(maxBytes) {
		return archive.FilteredTranscript{}, stat, errTranscriptTooLarge
	}
	f, ok := adapter.(agentapi.TranscriptFilter)
	if !ok {
		return archive.FilteredTranscript{}, stat, errors.New("native filter input port required")
	}
	out, err := f.Filter(ctx, agentapi.NativeInput{File: file}, agentapi.FilterContext{StartedAt: reg.SessionStartedAt, Limits: agentapi.ReadLimits{RawBytes: maxRawBytes(maxBytes), RecordBytes: recordLimit}})
	if err != nil {
		return out, stat, translateSourceError(err)
	}
	return out, stat, checkFilteredSize(out, maxBytes)
}

// discoveryReaderAt checks cancellation while validating admitted metadata.
type discoveryReaderAt struct {
	ctx  context.Context
	file io.ReaderAt
}

func (r discoveryReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.ReadAt(p, off)
}

// validateDiscoveryInput checks immutable admission facts before filtering the
// same verified handle. It never reopens the source path.
func validateDiscoveryInput(ctx context.Context, file agentapi.FileInput, reg archive.SessionRegistration) error {
	if file == nil {
		return errors.New("discovery source requires verified file input")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	header := sourcefacts.ReadCodexHeader(io.NewSectionReader(discoveryReaderAt{ctx: ctx, file: file}, 0, file.Length()), reg.TranscriptPath)
	var producerSource string
	_ = json.Unmarshal(header.Meta.Source, &producerSource)
	if header.Outcome != "native_format" || header.Meta.ID != reg.NativeSessionID || header.Meta.Cwd != reg.DiscoveryCwd || !header.Started.Equal(reg.SessionStartedAt) || header.Meta.Version != reg.Harness.Version || header.Meta.Originator != reg.DiscoveryProducerOriginator || producerSource != reg.DiscoveryProducerSource {
		return errors.New("discovery source identity changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return file.Check()
}
