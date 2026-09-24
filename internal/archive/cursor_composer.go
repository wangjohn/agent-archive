package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CursorComposer is one Cursor chat as read from state.vscdb: its
// composerData value and its messages in header order. A message whose row
// is missing has a nil Value.
type CursorComposer struct {
	Composer json.RawMessage
	Bubbles  []CursorBubble
}

// CursorBubble is one message row: the bubble ID from the chat's header and
// the bubbleId:<chat>:<message> value, nil when the row is missing.
type CursorBubble struct {
	ID    string
	Value json.RawMessage
}

// cursorComposerFormat is the source format of a chat filtered from Cursor's
// database, distinct from the hook-provided cursor-jsonl and cursor-text.
const cursorComposerFormat = "cursor-composer"

// maxCursorComposerVersion and maxCursorBubbleVersion are the newest _v this
// filter knows on a composerData value and on a message. A newer or missing
// _v is refused rather than guessed at (spec phase 2, decision 6).
// maxCursorComposerVersion matches the backfill count's newest known version.
const (
	maxCursorComposerVersion = 18
	maxCursorBubbleVersion   = 3
)

// Cursor's message types: 1 is the person, 2 is the assistant. Any other type
// is omitted and counted.
const (
	cursorBubbleUser      = 1
	cursorBubbleAssistant = 2
)

// errCursorFormatUnknown is an ErrUnsafeSourceFormat, so callers classify it
// as they do any other source they cannot safely filter.
var errCursorFormatUnknown = fmt.Errorf("cursor composer format version is not known: %w", ErrUnsafeSourceFormat)

// cursorComposerConsumed are the composerData keys the filter reads for
// structure and identity. They are not retained as fields, but nothing they
// hold is lost either, so they are not reported as omitted.
var cursorComposerConsumed = map[string]bool{
	"_v": true, "composerId": true, "createdAt": true,
	"fullConversationHeadersOnly": true, "conversation": true,
}

// cursorBubbleConsumed are the message keys the filter maps onto a record
// (see cursorBubbleRecord). richText is the editor's structured copy of text
// and is consumed only when text is present.
var cursorBubbleConsumed = map[string]bool{
	"_v": true, "bubbleId": true, "type": true, "text": true,
	"toolFormerData": true, "toolResults": true, "tokenCount": true, "modelInfo": true,
	"createdAt": true, "startedAtMs": true, "completedAtMs": true, "requestId": true,
}

// cursorContextKeys are the context payloads Cursor attaches to a message:
// code, files, diffs, images, and logs gathered for the model, not what the
// person or the assistant wrote. They are dropped by design (spec phase 2,
// decision 5) and reported by name under their own gap.
var cursorContextKeys = map[string]bool{
	"codebaseContextChunks": true, "attachedCodeChunks": true, "originalFileStates": true,
	"diffHistories": true, "images": true, "consoleLogs": true, "recentlyViewedFiles": true,
	"relevantFiles": true, "attachedFolders": true, "attachedFoldersNew": true,
	"attachedFileCodeChunksUris": true, "externalLinks": true, "docsReferences": true,
	"webReferences": true, "gitDiffs": true, "lints": true, "context": true,
	"contextPieces": true, "cursorRules": true, "summarizedComposers": true,
	"suggestedCodeBlocks": true, "fileDiffTrajectories": true, "workspaceUris": true,
}

// cursorHiddenKeys carry the model's reasoning, which no adapter retains.
var cursorHiddenKeys = map[string]bool{"thinking": true, "allThinkingBlocks": true}

// cursorComposerFilter accumulates one FilterComposer call.
type cursorComposerFilter struct {
	result                          FilteredTranscript
	gapSet                          map[string]bool
	omitted, denied, context        keyNameSet
	missing, blobMessages           int
	unknownType, unsupportedEntries int
	messages                        int
	composerBlob                    bool
}

func (f *cursorComposerFilter) addGap(code string, _ int, detail string) {
	key := code + ":" + detail
	if !f.gapSet[key] {
		f.gapSet[key] = true
		f.result.Gaps = append(f.result.Gaps, CaptureGap{Code: code, Detail: detail})
	}
}

// FilterComposer filters one chat from Cursor's database into native records,
// format "cursor-composer", through an allowlist (spec phase 2, decisions 5
// and 6). It writes one session record carrying the chat's ID and creation
// time, then one record per message in header order:
//
//	{"type":"session","session_id":…,"timestamp":…}
//	{"role":"user"|"assistant","id":…,"timestamp":…,"model":…,"requestId":…,
//	 "started_at_ms":…,"completed_at_ms":…,"usage":{"input_tokens":…,"output_tokens":…},
//	 "message":{"content":[{"type":"text","text":…},
//	   {"type":"tool_use","id":…,"name":…,"input":{…}},
//	   {"type":"tool_result","tool_use_id":…,"content":…,"status":…,"is_error":…}]}}
//
// Every record then goes through the same sanitizer as a JSONL record, so
// tool arguments, redaction, injected-instruction stripping, and the string
// cap apply exactly as they do to other adapters. Context payloads, reasoning,
// and every field not mapped above are dropped and reported by key name.
//
// It refuses (ErrUnsafeSourceFormat) a chat or message whose _v is missing or
// newer than this filter knows. A message with no row, one whose content
// lives in blobs this filter does not read, one of an unknown type, and an
// inline conversation entry of another shape are each omitted and counted in
// a gap. Records are stable once a message is complete: nothing that changes
// as the chat is used later (lastUpdatedAt, the chat's current model) is
// written into them.
func (CursorAdapter) FilterComposer(c CursorComposer) (FilteredTranscript, error) {
	if len(c.Composer) > maxRecordBytes {
		return FilteredTranscript{}, ErrRecordTooLarge
	}
	var composer map[string]any
	if err := json.Unmarshal(c.Composer, &composer); err != nil || composer == nil {
		return FilteredTranscript{}, &FilterError{Reason: "cursor composer is not valid JSON"}
	}
	if !knownCursorVersion(composer["_v"], maxCursorComposerVersion) {
		return FilteredTranscript{}, errCursorFormatUnknown
	}
	composerID, _ := composer["composerId"].(string)
	if strings.TrimSpace(composerID) == "" {
		return FilteredTranscript{}, &FilterError{Reason: "cursor composer has no composer ID"}
	}
	f := &cursorComposerFilter{
		result: FilteredTranscript{Format: cursorComposerFormat, SessionIDs: []string{composerID}},
		gapSet: map[string]bool{},
	}
	createdAt, hasCreatedAt := cursorTime(composer["createdAt"])
	if hasCreatedAt {
		f.result.NativeStartAt, f.result.FirstEventAt, f.result.NativeStartComplete = createdAt, createdAt, true
	}
	for _, key := range sortedKeys(composer) {
		switch {
		case cursorComposerConsumed[key]:
		case isCursorBlobKey(key):
			if key != "blobEncryptionKey" && nonEmptyValue(composer[key]) {
				f.composerBlob = true
			}
		default:
			f.omitted.add(key)
		}
	}
	session := map[string]any{"type": "session", "session_id": composerID}
	if hasCreatedAt {
		session["timestamp"] = createdAt.Format(time.RFC3339Nano)
	}
	if err := f.retain(session); err != nil {
		return FilteredTranscript{}, err
	}

	headers, _ := composer["fullConversationHeadersOnly"].([]any)
	var err error
	if len(headers) > 0 || len(c.Bubbles) > 0 {
		err = f.filterHeaderMessages(headers, c.Bubbles)
	} else if conversation, _ := composer["conversation"].([]any); len(conversation) > 0 {
		err = f.filterInlineConversation(conversation)
	}
	if err != nil {
		return FilteredTranscript{}, err
	}
	if !f.result.NativeStartComplete && !f.result.NativeStartAt.IsZero() {
		f.result.FirstEventAt = f.result.NativeStartAt
	}
	f.finishGaps()
	return f.result, nil
}

// filterHeaderMessages filters the messages listed in
// fullConversationHeadersOnly, which the reader supplies as bubbles in the
// same order. A list that does not match the headers is refused: the reader
// and the chat disagree about what the conversation is.
func (f *cursorComposerFilter) filterHeaderMessages(headers []any, bubbles []CursorBubble) error {
	if len(headers) != len(bubbles) {
		return &FilterError{Reason: "cursor composer messages do not match its headers"}
	}
	var lastAt time.Time
	for i, rawHeader := range headers {
		header, _ := rawHeader.(map[string]any)
		headerID, _ := header["bubbleId"].(string)
		if header == nil || headerID == "" || headerID != bubbles[i].ID {
			return &FilterError{Reason: "cursor composer messages do not match its headers"}
		}
		f.messages++
		headerType, hasHeaderType := cursorInt(header["type"])
		value := bytes.TrimSpace(bubbles[i].Value)
		if len(value) == 0 || string(value) == "null" {
			f.missing++
			if at := cursorMessageTime(header); !at.IsZero() {
				lastAt = at
			}
			continue
		}
		if len(value) > maxRecordBytes {
			return ErrRecordTooLarge
		}
		var bubble map[string]any
		if err := json.Unmarshal(value, &bubble); err != nil || bubble == nil {
			return &FilterError{Reason: "cursor message is not valid JSON"}
		}
		if !knownCursorVersion(bubble["_v"], maxCursorBubbleVersion) {
			return errCursorFormatUnknown
		}
		if bubbleType, ok := cursorInt(bubble["type"]); ok && hasHeaderType && bubbleType != headerType {
			// The header and the row disagree about who wrote the message.
			f.unknownType++
			continue
		}
		at, err := f.filterBubble(bubble)
		if err != nil {
			return err
		}
		if at.IsZero() {
			at = cursorMessageTime(header)
		}
		if !at.IsZero() {
			lastAt = at
		}
	}
	f.result.NativeEndAt = lastAt
	return nil
}

// filterInlineConversation filters an older chat that keeps its messages
// inline in "conversation" rather than in message rows. An entry in the
// message shape is filtered like a row; any other entry is counted.
func (f *cursorComposerFilter) filterInlineConversation(conversation []any) error {
	var lastAt time.Time
	for _, raw := range conversation {
		f.messages++
		bubble, _ := raw.(map[string]any)
		if id, _ := bubble["bubbleId"].(string); bubble == nil || id == "" {
			f.unsupportedEntries++
			continue
		}
		if _, ok := cursorInt(bubble["type"]); !ok {
			f.unsupportedEntries++
			continue
		}
		at, err := f.filterBubble(bubble)
		if err != nil {
			return err
		}
		if !at.IsZero() {
			lastAt = at
		}
	}
	f.result.NativeEndAt = lastAt
	return nil
}

// filterBubble retains one message and returns its time: completedAtMs, else
// createdAt, else zero.
func (f *cursorComposerFilter) filterBubble(bubble map[string]any) (time.Time, error) {
	if !knownCursorVersion(bubble["_v"], maxCursorBubbleVersion) {
		return time.Time{}, errCursorFormatUnknown
	}
	bubbleType, _ := cursorInt(bubble["type"])
	var role string
	switch bubbleType {
	case cursorBubbleUser:
		role = "user"
	case cursorBubbleAssistant:
		role = "assistant"
	default:
		f.unknownType++
		return time.Time{}, nil
	}
	record, blob := f.bubbleRecord(bubble, role)
	if blob {
		f.blobMessages++
	}
	created, _ := cursorTime(bubble["createdAt"])
	if !f.result.NativeStartComplete && !created.IsZero() && (f.result.NativeStartAt.IsZero() || created.Before(f.result.NativeStartAt)) {
		f.result.NativeStartAt = created
	}
	if err := f.retain(record); err != nil {
		return time.Time{}, err
	}
	return cursorMessageTime(bubble), nil
}

// bubbleRecord maps one message onto the record shape FilterComposer
// documents, reporting every key it does not map, and whether the message
// references blob content.
func (f *cursorComposerFilter) bubbleRecord(bubble map[string]any, role string) (map[string]any, bool) {
	record := map[string]any{"role": role}
	if id, ok := bubble["bubbleId"].(string); ok {
		record["id"] = id
	}
	if created, ok := cursorTime(bubble["createdAt"]); ok {
		record["timestamp"] = created.Format(time.RFC3339Nano)
	}
	if requestID, ok := bubble["requestId"].(string); ok && requestID != "" {
		record["requestId"] = requestID
	}
	for key, out := range map[string]string{"startedAtMs": "started_at_ms", "completedAtMs": "completed_at_ms"} {
		if ms, ok := bubble[key].(float64); ok && ms > 0 {
			record[out] = ms
		}
	}
	if info, ok := bubble["modelInfo"].(map[string]any); ok {
		for _, key := range sortedKeys(info) {
			if name, isString := info[key].(string); key == "modelName" && isString && name != "" {
				record["model"] = name
			} else if key != "modelName" {
				f.omitted.add(key)
			}
		}
	}
	if usage := f.cursorUsage(bubble["tokenCount"]); usage != nil {
		record["usage"] = usage
	}

	var content []any
	text, _ := bubble["text"].(string)
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	content = append(content, f.toolFormerBlocks(bubble["toolFormerData"])...)
	content = append(content, f.toolResultBlocks(bubble["toolResults"])...)
	if len(content) > 0 {
		record["message"] = map[string]any{"content": content}
	}

	blob := false
	for _, key := range sortedKeys(bubble) {
		switch {
		case cursorBubbleConsumed[key]:
		case key == "richText" && text != "":
		case cursorContextKeys[key]:
			if nonEmptyValue(bubble[key]) {
				f.context.add(key)
			}
		case cursorHiddenKeys[key]:
			if nonEmptyValue(bubble[key]) {
				f.addGap("hidden_instruction_omitted", 0, "reasoning omitted")
			}
		case isCursorBlobKey(key):
			if nonEmptyValue(bubble[key]) {
				blob = true
			}
		default:
			f.omitted.add(key)
		}
	}
	return record, blob
}

// cursorUsage maps a message's tokenCount onto the usage shape the parser
// sums. Cursor writes zeros on messages it did not meter, so an all-zero count
// is not retained as an observed zero.
func (f *cursorComposerFilter) cursorUsage(raw any) map[string]any {
	count, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	usage := map[string]any{}
	for _, key := range sortedKeys(count) {
		value, isNumber := count[key].(float64)
		switch {
		case key == "inputTokens" && isNumber:
			usage["input_tokens"] = value
		case key == "outputTokens" && isNumber:
			usage["output_tokens"] = value
		default:
			f.omitted.add(key)
		}
	}
	if in, out := usage["input_tokens"], usage["output_tokens"]; (in == nil || in == 0.0) && (out == nil || out == 0.0) {
		return nil
	}
	return usage
}

// toolFormerBlocks maps a message's toolFormerData (the tool call the
// assistant made and, once it ran, its result) onto a tool_use block and a
// tool_result block. Arguments come from rawArgs, a JSON string, or params;
// arguments that do not decode to an object are kept as one string.
func (f *cursorComposerFilter) toolFormerBlocks(raw any) []any {
	tool, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	callID, _ := tool["toolCallId"].(string)
	name, _ := tool["name"].(string)
	call := map[string]any{"type": "tool_use"}
	if callID != "" {
		call["id"] = callID
	}
	if name != "" {
		call["name"] = name
	}
	if input, ok := cursorToolArguments(tool["rawArgs"]); ok {
		call["input"] = input
	} else if input, ok := cursorToolArguments(tool["params"]); ok {
		call["input"] = input
	}
	var blocks []any
	if len(call) > 1 {
		blocks = append(blocks, call)
	}
	status, _ := tool["status"].(string)
	if output, ok := cursorToolOutput(tool["result"]); ok {
		result := map[string]any{"type": "tool_result", "content": output}
		if callID != "" {
			result["tool_use_id"] = callID
		}
		if status != "" {
			result["status"] = status
		}
		if strings.EqualFold(status, "error") {
			result["is_error"] = true
		}
		blocks = append(blocks, result)
	}
	for _, key := range sortedKeys(tool) {
		switch key {
		case "toolCallId", "name", "rawArgs", "params", "result", "status":
		default:
			f.omitted.add(key)
		}
	}
	return blocks
}

// toolResultBlocks maps the entries of a message's toolResults onto
// tool_result blocks. An entry keeps its call ID, tool name, and result; any
// other member is reported by name.
func (f *cursorComposerFilter) toolResultBlocks(raw any) []any {
	entries, _ := raw.([]any)
	var blocks []any
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			f.addGap("unsupported_value_omitted", 0, "value omitted")
			continue
		}
		block := map[string]any{"type": "tool_result"}
		for _, key := range sortedKeys(entry) {
			switch key {
			case "toolCallId":
				if id, ok := entry[key].(string); ok && id != "" {
					block["tool_use_id"] = id
				}
			case "name", "toolName":
				if name, ok := entry[key].(string); ok && name != "" {
					block["name"] = name
				}
			case "result":
				if output, ok := cursorToolOutput(entry[key]); ok {
					block["content"] = output
				}
			default:
				f.omitted.add(key)
			}
		}
		if len(block) > 1 {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

// retain sanitizes one built record exactly as filterJSONL sanitizes a native
// one and appends it.
func (f *cursorComposerFilter) retain(record map[string]any) error {
	state := sanitizeState{addGap: f.addGap, omittedKey: f.omitted.add, deniedKey: f.denied.add}
	safe, keep := sanitizeObject(record, &state)
	if !keep {
		return nil
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return &FilterError{Reason: "safe record cannot be encoded"}
	}
	f.result.Records = append(f.result.Records, encoded)
	f.result.Boundary.RetainedRecords++
	f.result.Boundary.RetainedBytes += len(encoded)
	return nil
}

// finishGaps adds the counted and named gaps, then orders every gap by code as
// filterJSONL does.
func (f *cursorComposerFilter) finishGaps() {
	if f.missing > 0 {
		f.addGap("cursor_bubble_missing", 0, fmt.Sprintf("%d of %d messages have no message row", f.missing, f.messages))
	}
	if f.blobMessages > 0 {
		f.addGap("cursor_blob_content_unavailable", 0, fmt.Sprintf("%d of %d messages reference content blobs, which are not read", f.blobMessages, f.messages))
	}
	if f.composerBlob {
		f.addGap("cursor_blob_content_unavailable", 0, "chat references content blobs, which are not read")
	}
	if f.unknownType > 0 {
		f.addGap("cursor_message_type_unknown", 0, fmt.Sprintf("%d of %d messages have an unknown or inconsistent type", f.unknownType, f.messages))
	}
	if f.unsupportedEntries > 0 {
		f.addGap("cursor_conversation_entry_unsupported", 0, fmt.Sprintf("%d of %d inline conversation entries are not in the message shape", f.unsupportedEntries, f.messages))
	}
	if detail := f.context.detail("omitted context fields: "); detail != "" {
		f.addGap("cursor_context_omitted", 0, detail)
	}
	if detail := f.omitted.detail("omitted keys: "); detail != "" {
		f.addGap("unknown_field_omitted", 0, detail)
	}
	if detail := f.denied.detail(deniedToolArgumentIntro); detail != "" {
		f.addGap("sensitive_or_hidden_field_omitted", 0, detail)
	}
	sort.SliceStable(f.result.Gaps, func(i, j int) bool { return f.result.Gaps[i].Code < f.result.Gaps[j].Code })
}

// knownCursorVersion reports whether a _v value is a positive integer no newer
// than newest. A missing _v is not known.
func knownCursorVersion(raw any, newest int) bool {
	v, ok := cursorInt(raw)
	return ok && v >= 1 && v <= newest
}

// cursorInt reads a JSON number that is a whole number.
func cursorInt(raw any) (int, bool) {
	value, ok := raw.(float64)
	if !ok || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}

// cursorTime reads a Cursor timestamp: milliseconds since the epoch (as
// composerData and the *Ms fields write it) or an RFC 3339 string (as some
// message rows write createdAt).
func cursorTime(raw any) (time.Time, bool) {
	switch value := raw.(type) {
	case float64:
		if value > 0 {
			return time.UnixMilli(int64(value)).UTC(), true
		}
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// cursorMessageTime is when a message (or its header) finished: completedAtMs,
// else createdAt.
func cursorMessageTime(message map[string]any) time.Time {
	if at, ok := cursorTime(message["completedAtMs"]); ok {
		return at
	}
	at, _ := cursorTime(message["createdAt"])
	return at
}

// cursorToolArguments decodes a tool call's arguments: an object as is, or a
// JSON string that decodes to one. Any other non-empty string is kept whole,
// as Codex arguments are.
func cursorToolArguments(raw any) (any, bool) {
	switch value := raw.(type) {
	case map[string]any:
		return value, len(value) > 0
	case string:
		if value == "" {
			return nil, false
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(value), &decoded) == nil && decoded != nil {
			return decoded, true
		}
		return value, true
	}
	return nil, false
}

// cursorToolOutput reads a tool result as one string. Cursor stores most as a
// JSON string; a structured result is encoded to one, so the sanitizer treats
// it as the text it is rather than applying the key allowlist to it.
func cursorToolOutput(raw any) (string, bool) {
	switch value := raw.(type) {
	case nil:
		return "", false
	case string:
		return value, value != ""
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", false
		}
		return string(encoded), true
	}
}

// isCursorBlobKey reports whether a key refers to agentKv blob content.
func isCursorBlobKey(key string) bool {
	return strings.Contains(strings.ToLower(key), "blob")
}

// nonEmptyValue reports whether a JSON value carries anything.
func nonEmptyValue(raw any) bool {
	switch value := raw.(type) {
	case nil:
		return false
	case bool:
		return value
	case float64:
		return value != 0
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	}
	return true
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
