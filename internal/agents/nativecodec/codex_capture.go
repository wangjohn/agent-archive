package nativecodec

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// FilterCodexCaptureJSONL refuses partial related histories before privacy
// filtering drops their metadata. Refiltering retained sources stays separate.
func FilterCodexCaptureJSONL(r io.Reader, filename string) (archive.FilteredTranscript, error) {
	return filterJSONL(r, "codex-jsonl", map[string]bool{
		"session_meta": true, "turn_context": true, "response_item": true,
		"event_msg": true, "message": true, "token_usage_record": true,
	}, nil, func(line []byte) error { return codexCaptureMetadata(line, filename) })
}

func codexCaptureMetadata(line []byte, filename string) error {
	var record struct {
		Payload codexmeta.CodexMeta `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil {
		return &archive.FilterError{Reason: "invalid Codex metadata"}
	}
	m := record.Payload
	facts, outcome := m.Relationships()
	if outcome != "" {
		return &archive.FilterError{Reason: string(outcome)}
	}
	if rollout := codexmeta.RolloutID(filename); rollout != "" && !strings.EqualFold(rollout, m.ID) {
		return archive.ErrRelatedHistory
	}
	if facts.Child || facts.ForkID != "" || facts.HistoryBase != nil {
		return archive.ErrRelatedHistory
	}
	if m.HistoryMode != "" && m.HistoryMode != codexmeta.CodexHistoryLegacy && m.HistoryMode != codexmeta.CodexHistoryPaginated {
		return &archive.FilterError{Reason: "unsupported Codex history"}
	}
	return nil
}
