package archive

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Filter 14 keeps the name the parent session gave a Claude Code subagent's
// task. The name is not in the subagent's transcript: Claude Code writes it
// to a sibling file, agent-<id>.meta.json, beside agent-<id>.jsonl. The
// collector reads that file when it filters the transcript and hands its
// bytes to ClaudeAdapter.FilterSubagentJSONL, which writes one synthetic
// record at the front of the filtered records:
//
//	{"type":"subagent-meta","description":…}
//
// Nothing else of the file is kept. worktreePath names a local path and
// agentType stays on this Mac; neither is read, and neither is reported as an
// omitted key, since the file is not part of the transcript.
//
// subagentMetaType is the type of that record.
const subagentMetaType = "subagent-meta"

// subagentDescriptionKey is the one key the record carries besides its type.
const subagentDescriptionKey = "description"

// MaxSubagentMetaBytes is the largest .meta.json file the collector reads.
// A real one holds three short fields; a larger file is not one, and is
// treated as absent.
const MaxSubagentMetaBytes = 16 * 1024

// maxSubagentDescriptionBytes bounds the description a subagent-meta record
// keeps. A task description is a few words (the parser cuts the name derived
// from it to 72 characters), so this is far smaller than the 64 KB a prompt
// may be, and a longer one is cut on a character boundary.
const maxSubagentDescriptionBytes = 512

// subagentMetaKeys are the keys a subagent-meta record may hold, which
// subagentMetaRecord rebuilds from typed values: its type and its description.
var subagentMetaKeys = map[string]bool{"type": true, subagentDescriptionKey: true}

// SubagentMetaPath is the .meta.json beside a Claude Code subagent
// transcript: agent-<id>.jsonl names agent-<id>.meta.json in the same
// directory. ok is false for any other file name, which is not a subagent
// transcript.
func SubagentMetaPath(transcriptPath string) (path string, ok bool) {
	dir, name := filepath.Split(transcriptPath)
	id, found := strings.CutPrefix(name, "agent-")
	if !found {
		return "", false
	}
	id, found = strings.CutSuffix(id, ".jsonl")
	if !found || id == "" {
		return "", false
	}
	return filepath.Join(dir, "agent-"+id+".meta.json"), true
}

// subagentMetaLead is the raw record FilterSubagentJSONL feeds the filter for
// the contents of a .meta.json: its description, as a subagent-meta record,
// and nothing else of the file. ok is false when the file holds no usable
// description: it is empty, oversized, not a JSON object, or its description
// is missing, not a string, or blank. The file is optional, so none of these
// is an error or a gap.
func subagentMetaLead(metaJSON []byte) (map[string]any, bool) {
	if len(metaJSON) == 0 || len(metaJSON) > MaxSubagentMetaBytes {
		return nil, false
	}
	var meta map[string]any
	if json.Unmarshal(metaJSON, &meta) != nil {
		return nil, false
	}
	description, _ := meta[subagentDescriptionKey].(string)
	if strings.TrimSpace(description) == "" {
		return nil, false
	}
	return map[string]any{"type": subagentMetaType, subagentDescriptionKey: description}, true
}

// subagentMetaRecord rebuilds a subagent-meta record from the one string it
// may keep, reporting the name of every other key through omit. It returns
// false when there is no description, or it is not a string or is blank.
// Only the type and the description are kept: no sessionId or timestamp, as
// the record is not part of the transcript.
func subagentMetaRecord(raw map[string]any, omit func(string)) (map[string]any, bool) {
	for _, key := range sortedKeys(raw) {
		if !subagentMetaKeys[key] {
			omit(key)
		}
	}
	description, ok := raw[subagentDescriptionKey].(string)
	if !ok || strings.TrimSpace(description) == "" {
		return nil, false
	}
	return map[string]any{"type": subagentMetaType, subagentDescriptionKey: description}, true
}

// boundSubagentDescription redacts a description as prompt text is and bounds
// it to maxSubagentDescriptionBytes. The text is redacted whole first, so a
// secret is never cut in half and left unrecognizable; the bounded text is
// then redacted again, as sanitizeValue repeats its own passes, until it is
// stable. ok is false when nothing is left, or the text cannot be made stable
// and short.
func boundSubagentDescription(text string, state *sanitizeState) (string, bool) {
	for range maxSanitizeStringPasses {
		safe, keep := sanitizeValue(text, state)
		next, isString := safe.(string)
		if !keep || !isString || strings.TrimSpace(next) == "" {
			return "", false
		}
		if len(next) <= maxSubagentDescriptionBytes {
			return next, true
		}
		state.addGap("content_truncated", state.record, "content truncated")
		text = TruncateUTF8(next, maxSubagentDescriptionBytes)
	}
	return "", false
}

// subagentMetaSlot admits one subagent-meta record to a transcript: the first
// it is given. A later one is dropped with an unsupported_value_omitted gap,
// which carries no content.
type subagentMetaSlot struct{ taken bool }

// filter is filterSubagentMeta for the first record the slot is given, and
// drops every later one.
func (s *subagentMetaSlot) filter(raw map[string]any, lineNo int, addGap func(string, int, string), omit func(string)) ([]byte, error) {
	if s.taken {
		addGap("unsupported_value_omitted", lineNo, "record omitted")
		return nil, nil
	}
	s.taken = true
	return filterSubagentMeta(raw, lineNo, addGap, omit)
}

// filterSubagentMeta filters one subagent-meta record and returns its
// encoding, or nil when the record is dropped, reported in an
// unsupported_value_omitted gap that carries no content.
func filterSubagentMeta(raw map[string]any, lineNo int, addGap func(string, int, string), omit func(string)) ([]byte, error) {
	label, ok := subagentMetaRecord(raw, omit)
	if !ok {
		addGap("unsupported_value_omitted", lineNo, "record omitted")
		return nil, nil
	}
	state := sanitizeState{record: lineNo, addGap: addGap, omittedKey: omit}
	description, ok := boundSubagentDescription(label[subagentDescriptionKey].(string), &state)
	if !ok {
		addGap("unsupported_value_omitted", lineNo, "record omitted")
		return nil, nil
	}
	encoded, err := json.Marshal(map[string]any{"type": subagentMetaType, subagentDescriptionKey: description})
	if err != nil {
		return nil, &FilterError{Reason: "safe record cannot be encoded"}
	}
	return encoded, nil
}

// WithoutSubagentMeta is the retained records of a claude-jsonl transcript
// without the subagent-meta record filter 14 may have written first. The
// record is the name the parent gave the task, a label the latest read of the
// .meta.json replaces, not evidence of the transcript: a subagent captured
// before its .meta.json existed, or whose description changed, still extends
// its earlier snapshot. Records in any other format are returned as given.
func WithoutSubagentMeta(format string, records []map[string]any) []map[string]any {
	if format == "claude-jsonl" && len(records) > 0 && records[0]["type"] == subagentMetaType {
		return records[1:]
	}
	return records
}
