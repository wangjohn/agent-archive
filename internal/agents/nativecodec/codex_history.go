package nativecodec

import "github.com/wangjohn/agent-archive/internal/archive"

// FilterCodexHistory applies the normal privacy policy to provider-validated frames.
// retained observes cumulative retained counts before requesting the next raw frame.
func FilterCodexHistory(next func() ([]byte, bool), readError func() error, retained func(int)) (archive.FilteredTranscript, error) {
	return filterRecordsObserved("codex-jsonl", map[string]bool{"session_meta": true, "turn_context": true, "response_item": true, "event_msg": true, "message": true, "token_usage_record": true}, nil, next, readError, nil, retained)
}
