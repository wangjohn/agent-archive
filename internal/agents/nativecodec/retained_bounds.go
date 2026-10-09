package nativecodec

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
)

// FilterClaudeRetainedJSONL applies the same Claude codec with an accumulation ceiling.
func FilterClaudeRetainedJSONL(r io.Reader, bound archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterClaudeJSONL(r, nil, bound)
}

// FilterCursorRetainedJSONL applies the same Cursor codec with an accumulation ceiling.
func FilterCursorRetainedJSONL(r io.Reader, bound archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterCursorJSONL(r, bound)
}
