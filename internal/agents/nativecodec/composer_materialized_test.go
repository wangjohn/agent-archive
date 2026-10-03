package nativecodec

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// CursorComposer is one Cursor chat as read from state.vscdb: its
// composerData value and its messages in header order. A message whose row
// is missing has a nil Value.
type CursorComposer struct {
	Composer json.RawMessage `json:"composer"`
	Bubbles  []CursorBubble  `json:"bubbles"`
}

// FilterComposer filters one chat from Cursor's database into native records,
// format "cursor-composer", through an allowlist (spec phase 2, decisions 5
// and 6). When the chat has at least one retained message it writes one
// session record carrying the chat's ID and creation time, then one record
// per message in header order:
//
//	{"type":"session","session_id":…,"timestamp":…,"name":…}
//	{"role":"user"|"assistant","id":…,"timestamp":…,"model":…,"requestId":…,
//	 "started_at_ms":…,"completed_at_ms":…,"usage":{"input_tokens":…,"output_tokens":…},
//	 "message":{"content":[{"type":"text","text":…},
//	   {"type":"tool_use","id":…,"name":…,"input":{…}},
//	   {"type":"tool_result","tool_use_id":…,"content":…,"status":…,"is_error":…}]}}
//
// The session record's name is the chat's own name, present only when Cursor
// has given the chat one; it is redacted like a message's text.
//
// Every record then goes through the same sanitizer as a JSONL record, so
// tool arguments, redaction, injected-instruction stripping, and the string
// cap apply exactly as they do to other adapters. Context payloads, reasoning,
// and every field not mapped above are dropped and reported by key name.
//
// It refuses (archive.ErrUnsafeSourceFormat) a chat or message whose _v is not the
// one version this filter knows. A message whose content lives in blobs this
// filter does not read, and one of an unknown type, are omitted and counted
// in a gap.
//
// Records are append-only across snapshots of a chat that is still in use, as
// far as the filter can make them: output stops at the first message in
// flight (see cursorGenerating), such as a reply still streaming or a tool
// call still running, and at the first message whose row is missing, belongs
// to another message, or disagrees with its header about its type. That
// message and everything after it are left for a later pass (and counted in
// cursor_incomplete_tail_omitted, besides the specific gap). Nothing that
// changes as the chat is merely used (lastUpdatedAt, the chat's current
// model) is written into a record. If Cursor rewrites a finished message the
// output changes; the collector, not the filter, handles that.
func (CursorAdapter) FilterComposer(c CursorComposer) (archive.FilteredTranscript, error) {
	i := 0
	return FilterComposerRecords(context.Background(), c.Composer, func(context.Context) (CursorBubble, bool, error) {
		if i == len(c.Bubbles) {
			return CursorBubble{}, false, nil
		}
		b := c.Bubbles[i]
		i++
		return b, true, nil
	})
}
