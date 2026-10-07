package nativecodec

import "github.com/wangjohn/agent-archive/internal/archive"

// FilterCodexHistoryEncoded borrows the caller's bounded encoder for safe records.
func FilterCodexHistoryEncoded(next func() ([]byte, bool), readError func() error, retained func(int), own func(string) bool, encoder func(map[string]any) ([]byte, error), beforeRecord func(int) (func(), error), bounds ...archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterRecordsObserved("codex-jsonl", map[string]bool{"session_meta": true, "turn_context": true, "response_item": true, "event_msg": true, "message": true, "token_usage_record": true}, nil, next, readError, nil, retained, own, encoder, beforeRecord, bounds...)
}
