package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Adapter filters one application's hook-provided JSONL transcript. Adapters
// are deliberately readers only; the collector owns paths, retries, and I/O.
type Adapter interface {
	Name() string
	Version() string
	FilterJSONL(io.Reader) (FilteredTranscript, error)
}

// FilterError means no new source bundle may be made from this input. It is
// intentionally distinct from ParseError, where a filtered source is still
// safe and valuable to retain.
type FilterError struct{ Reason string }

func (e *FilterError) Error() string { return "unsafe source format: " + e.Reason }

var ErrUnsafeSourceFormat = &FilterError{Reason: "no recognized safe records"}

// ErrRecordTooLarge means one JSONL record is longer than MaxRecordBytes. It is
// a FilterError like any other refusal, distinct so a caller can record it as
// the capture gap it is rather than a malformed transcript.
var ErrRecordTooLarge = &FilterError{Reason: "record exceeds the record size limit"}

// MaxRecordBytes is the largest single JSONL record the filter reads, and the
// collector's transcript size ceiling is defined from it (see
// collector.DefaultMaxTranscriptBytes), so any record inside a transcript the
// collector accepts can be read. Filter 4 and earlier stopped at 2 MB, which
// refused whole Claude Code sessions whose tool results are a few megabytes
// before filtering (the bulk is in toolUseResult, which the filter drops).
//
// Reading one record costs memory in proportion to its size: the scanner's
// buffer and the decoded JSON value both hold it, several times over at the
// limit. See docs/agent-archive-implementation.md for the measured ceiling.
const MaxRecordBytes = 64 * 1024 * 1024

// maxRecordBytes is MaxRecordBytes, as a variable only so a test can lower it.
var maxRecordBytes = MaxRecordBytes

const adapterVersion = "0.10.0"

// maxOmittedKeyNames bounds how many distinct omitted key names one filtered
// transcript reports, so a pathological source cannot grow the gap list.
const maxOmittedKeyNames = 64

// DefaultParserVersion is the source parser version reported by this bounded
// foundation. The parser is intentionally partial until fixture coverage proves
// a given native format more completely.
const DefaultParserVersion = "0.10.0"

// NewAdapter returns a privacy-first adapter by canonical harness name.
func NewAdapter(name string) (Adapter, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "codex":
		return CodexAdapter{}, nil
	case "claude", "claude-code":
		return ClaudeAdapter{}, nil
	case "cursor":
		return CursorAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported archive adapter %q", name)
	}
}

// CodexAdapter supports the conservative JSONL shapes observed by this
// foundation. Unsupported Codex record types are gaps, never pass-through.
type CodexAdapter struct{}

func (CodexAdapter) Name() string    { return "codex" }
func (CodexAdapter) Version() string { return adapterVersion }
func (CodexAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "codex-jsonl", map[string]bool{
		"session_meta": true, "turn_context": true, "response_item": true,
		"event_msg": true, "message": true, "token_usage_record": true,
	})
}

// ClaudeAdapter handles a small, explicit subset of Claude Code JSONL event
// types. It does not claim schema coverage for every installed version.
type ClaudeAdapter struct{}

func (ClaudeAdapter) Name() string    { return "claude" }
func (ClaudeAdapter) Version() string { return adapterVersion }
func (ClaudeAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "claude-jsonl", map[string]bool{
		"user": true, "assistant": true, "tool_use": true, "tool_result": true,
		"message": true, "summary": true,
	})
}

// CursorAdapter filters hook-provided JSONL records, a hook-provided text
// transcript (FilterText), and a chat read from Cursor's database
// (FilterComposer, in cursor_composer.go). Undocumented formats remain
// unsupported capture gaps upstream.
type CursorAdapter struct{}

func (CursorAdapter) Name() string    { return "cursor" }
func (CursorAdapter) Version() string { return adapterVersion }
func (CursorAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "cursor-jsonl", map[string]bool{
		"session": true, "message": true, "tool_call": true, "tool_result": true,
		"event": true, "turn_ended": true,
	})
}

// FilterText retains a hook-provided Cursor text transcript only when the hook
// has established that this is a fresh eligible session. It labels the source
// as text rather than fabricating message events from unstructured content.
//
// The transcript as a whole is bounded by the record size limit (a text
// transcript is one unit, like one JSONL record); a longer one fails with
// ErrRecordTooLarge, which the collector records as a capture gap. Filter 6
// and earlier stopped at 2 MB.
//
// Each visible role section — a `user:`, `assistant:`, or `tool:` line and the
// continuation lines under it — is sanitized on its own, so redaction and the
// 64 KB string cap apply per message, as they do to JSONL records. Filter 6
// sanitized the whole joined text as one string, which truncated any text
// transcript over 64 KB to its first 64 KB. The sanitized sections are joined
// again in their original order, one line per original line, so the result is
// read back by the same section prefixes (see textSectionPrefixes). Hidden
// sections (`system:`, `developer:`, `thinking:`, `analysis:`) and their
// continuation lines are omitted. Each gap is recorded once.
//
// The collector's rewrite guard compares the retained text by prefix across
// passes. Per-section sanitizing keeps every completed section's bytes
// stable, but the last section, if it is still being written, can change
// bytes it already produced once more of it lands (a credential that only
// matches when complete, or an injected block whose stripping trims the
// section's edges). That is a property of sanitizing a growing string, not
// of this function, and a text transcript offers no record boundary to stop
// short of.
// visibleTextRoles and hiddenTextRoles are the role headers of a Cursor
// text transcript: a visible section is retained, a hidden one omitted.
var (
	visibleTextRoles = map[string]bool{"user": true, "assistant": true, "tool": true}
	hiddenTextRoles  = map[string]bool{"system": true, "developer": true, "thinking": true, "analysis": true}
)

// textRoleHeader reports whether line starts a role section of a Cursor text
// transcript, and returns the role and the text after the header. A header
// is how Cursor writes one: the role name (in any case) and a colon at
// column 0, then a space or the end of the line; the role is returned in
// lower case. An indented "user:" is content, such as a YAML key in tool
// output, and must never start a turn or hide what follows it.
func textRoleHeader(line string) (role, rest string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	colon := strings.IndexByte(line, ':')
	if colon <= 0 {
		return "", "", false
	}
	role, rest = strings.ToLower(line[:colon]), line[colon+1:]
	if !visibleTextRoles[role] && !hiddenTextRoles[role] {
		return "", "", false
	}
	if rest != "" && rest[0] != ' ' {
		return "", "", false
	}
	return role, strings.TrimPrefix(rest, " "), true
}

func (CursorAdapter) FilterText(r io.Reader, freshStartedAt time.Time) (FilteredTranscript, error) {
	if freshStartedAt.IsZero() {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no reliable fresh-session start"}
	}
	content, err := io.ReadAll(io.LimitReader(r, int64(maxRecordBytes)+1))
	if err != nil {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript cannot be read"}
	}
	if len(content) > maxRecordBytes {
		return FilteredTranscript{}, ErrRecordTooLarge
	}
	result := FilteredTranscript{Format: "cursor-text", FirstEventAt: freshStartedAt.UTC()}
	gapSet := map[CaptureGap]bool{}
	addGap := func(code, detail string) {
		gap := CaptureGap{Code: code, Detail: detail}
		if !gapSet[gap] {
			gapSet[gap] = true
			result.Gaps = append(result.Gaps, gap)
		}
	}
	addGap("text_structure_partial", "Cursor role sections retained without manufactured events")

	// Split into sections, keeping each visible section's lines as they were.
	var sections [][]string
	hidden := false
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		role, _, header := textRoleHeader(line)
		switch {
		case header && hiddenTextRoles[role]:
			hidden = true
			addGap("hidden_instruction_omitted", "text section omitted")
		case header:
			hidden = false
			sections = append(sections, []string{line})
		case hidden:
			// continuation line of an already-hidden section; omit.
		case len(sections) > 0:
			// continuation line of the current visible section's message body.
			sections[len(sections)-1] = append(sections[len(sections)-1], line)
		default:
			return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has unrecognized role section"}
		}
	}
	if len(sections) == 0 {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable visible sections"}
	}
	state := sanitizeState{addGap: func(code string, _ int, detail string) { addGap(code, detail) }}
	retained := make([]string, 0, len(sections))
	for _, section := range sections {
		safe, keep := sanitizeValue(strings.Join(section, "\n"), &state)
		if !keep {
			continue
		}
		text, ok := safe.(string)
		if !ok {
			return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript is not text"}
		}
		retained = append(retained, text)
	}
	if len(retained) == 0 {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable content"}
	}
	text := strings.Join(retained, "\n")
	result.Text = []string{text}
	result.Boundary.RetainedBytes = len(text)
	return result, nil
}

var allowedKeys = map[string]bool{
	"type": true, "id": true, "uuid": true, "session_id": true, "parent_id": true,
	"parent_uuid": true, "parentuuid": true, "timestamp": true, "created_at": true, "updated_at": true,
	"cwd": true, "model": true, "model_provider": true, "role": true, "content": true,
	"channel": true,
	"text":    true, "message": true, "item": true, "event": true, "payload": true,
	"tool_name": true, "tool_input": true, "tool_output": true, "tool_use": true,
	"tool_result": true, "call_id": true, "input": true, "output": true, "result": true, "arguments": true,
	"command": true, "path": true, "query": true, "url": true, "description": true,
	"status": true, "event_name": true, "turn_id": true, "reasoning_effort": true, "name": true, "items": true,
	"sha256": true, "message_id": true, "settings": true, "model_id": true, "discovered": true, "installed": true, "snapshot": true, "source": true,
	"coverage": true, "skills": true, "redacted": true, "observed_at": true,
	"model_params": true, "value": true, "cli_version": true, "agent_id": true,
	// "version" is the per-record Claude Code build stamp; it attributes a
	// published capture to an installed version. Values still pass sanitizeValue.
	"version":     true,
	"uncertainty": true, "scope": true, "original_bytes": true, "event_id": true,
	"truncated": true, "omitted_count": true, "snapshot_omitted_count": true, "inventory_complete": true, "root_status": true,
	"gaps":  true,
	"skill": true,
	// Claude Code marks a subagent's records with is_sidechain. Retaining the
	// flag lets a parent's normalized view exclude any inlined child records
	// from its own counts; the child is archived as its own session.
	"is_sidechain": true, "issidechain": true,
	"file_path":          true,
	"archive_session_id": true, "relationship": true,
	// Filter 3 additions. A tool result can only be joined back to the call it
	// answers through tool_use_id, and a turn's own identity (sessionId,
	// requestId, gitBranch) is what lets a reader place a record in its
	// session and working branch. usage and the Codex *_token_usage subtrees
	// are retained as numbers only (see numericSubtreeKeys).
	"tool_use_id": true, "tooluseid": true, "is_error": true, "stop_reason": true,
	"usage": true, "sessionid": true, "requestid": true, "gitbranch": true,
	// Codex token accounting records.
	"info": true, "total_token_usage": true, "last_token_usage": true,
	"turn_token_usage": true, "thread_token_usage": true, "last_agent_message": true,
	"thread_id": true, "root_turn_id": true, "completed_at_ms": true, "started_at_ms": true,
	// Filter 4: Claude Code marks harness-written user records (an expanded
	// skill or slash command, a local-command caveat) with isMeta. The flag is
	// what tells a parser such a record is not a human prompt; the record's
	// text itself is stripped (see stripMetaRecordText).
	"ismeta": true,
	// Filter 5: after /compact or auto-compaction Claude Code writes a user
	// record carrying a model-written summary of the earlier conversation,
	// marked isCompactSummary (and usually isVisibleInTranscriptOnly). The
	// flags tell a parser it is not a prompt. Unlike an isMeta record, the
	// summary's text is kept: it is model output, useful for a handoff. Both
	// are admitted as booleans only (see booleanFlagKeys).
	"iscompactsummary": true, "isvisibleintranscriptonly": true,
	// Filter 6: Claude Code says who produced a user record. A person's prompt
	// carries origin.kind "human"; a background-task completion carries
	// "task-notification" and promptSource "system". origin is retained only
	// as its kind string (see originKindOnly) and promptSource only as a
	// string, so a parser can tell a notification from a prompt.
	"origin": true, "promptsource": true,
}

// booleanFlagKeys are allowed only as the boolean flag the harness writes. Any
// other value under one of these names is prose the allowlist never retained.
var booleanFlagKeys = map[string]bool{"ismeta": true, "iscompactsummary": true, "isvisibleintranscriptonly": true}

// toolArgumentKeys name the subtrees which carry a tool call's own arguments.
// Filter 2 applied allowedKeys recursively inside them, which dropped every
// Edit old_string/new_string, Agent prompt, Grep pattern, and MCP argument and
// left tool evidence unusable. Inside these subtrees every argument name is
// retained; blockedKeys, value redaction (redactSensitive), the string cap, and the
// hidden role/channel rules all still apply to the values. Codex's
// payload.input is covered by the same "input" entry.
var toolArgumentKeys = map[string]bool{"input": true, "arguments": true, "tool_input": true}

// Retaining tool arguments wholesale has two exceptions, applied at every
// depth of a tool-argument subtree. The argument's key name is recorded in a
// gap; its value is never retained.
//
// typedInputArgumentKeys are the arguments which carry text a tool types or
// submits outward — into a browser field, a terminal, or a device — when the
// tool's name (`name` or `tool_name` beside the subtree) is one of
// typedInputToolNames, ends with `_` followed by one of them (an MCP tool such
// as `mcp__browser__computer`), or ends with one of typedInputToolSuffixes.
// What was typed into a login form is exactly the value a transcript must not
// keep, and the tool's own name is the only signal of that.
//
// credentialKeyFragments drop any argument whose lowercase key contains one
// of them, for every tool. This is broader than blockedKeys (which need an
// exact name) and knowingly catches budgets such as `max_tokens`.
var (
	typedInputArgumentKeys  = map[string]bool{"text": true, "value": true, "values": true}
	typedInputToolNames     = []string{"type", "form_input", "computer", "key", "enter_verification_code", "autofill_credential"}
	typedInputToolSuffixes  = []string{"_type", "_input", "_fill"}
	credentialKeyFragments  = []string{"password", "secret", "token", "credential", "api_key", "apikey", "cookie", "authorization"}
	deniedToolArgumentIntro = "omitted tool argument keys: "
)

func isTypedInputTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	for _, candidate := range typedInputToolNames {
		if lower == candidate || strings.HasSuffix(lower, "_"+candidate) {
			return true
		}
	}
	for _, suffix := range typedInputToolSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// deniedToolArgument reports whether a tool argument's value must be dropped
// even though the subtree otherwise retains every key.
func deniedToolArgument(key, toolName string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range credentialKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return typedInputArgumentKeys[lower] && isTypedInputTool(toolName)
}

// keyNameSet collects distinct key names for one summary gap, capped so a
// pathological source cannot grow the gap list. Names only, never values.
type keyNameSet struct {
	names  map[string]bool
	capped bool
}

func (s *keyNameSet) add(key string) {
	if s.names == nil {
		s.names = map[string]bool{}
	}
	if s.names[key] {
		return
	}
	if len(s.names) >= maxOmittedKeyNames {
		s.capped = true
		return
	}
	s.names[key] = true
}

// detail renders the sorted names after intro, noting when the cap was hit,
// or returns "" when nothing was collected.
func (s *keyNameSet) detail(intro string) string {
	if len(s.names) == 0 {
		return ""
	}
	names := make([]string, 0, len(s.names))
	for key := range s.names {
		names = append(names, key)
	}
	sort.Strings(names)
	detail := intro + strings.Join(names, ", ")
	if s.capped {
		detail += "; further key names omitted"
	}
	return detail
}

// numericSubtreeKeys name subtrees retained for their numbers only: token
// accounting carries no prose, so anything in them which is not a number (or a
// nested object or array of numbers) is omitted with its key name recorded.
var numericSubtreeKeys = map[string]bool{
	"usage": true, "total_token_usage": true, "last_token_usage": true,
	"turn_token_usage": true, "thread_token_usage": true,
}

// captureGapKeys are additionally allowed inside a capture_gap evidence
// payload, whose whole content is an archive-authored code and its fixed
// description. They are deliberately not in allowedKeys: `detail` is a
// common free-text field name in native transcripts, and sanitizeObject
// recurses, so allowing it globally would retain arbitrary nested prose.
var captureGapKeys = map[string]bool{"code": true, "detail": true}

var blockedKeys = map[string]bool{
	"api_key": true, "apikey": true, "access_key": true, "secret": true,
	"secret_key": true, "password": true, "authorization": true, "token": true,
	"cookie": true, "set_cookie": true, "system": true, "developer": true,
	"instructions": true, "reasoning": true, "analysis": true, "encrypted_content": true,
	"image": true, "images": true, "audio": true, "binary": true, "attachment": true,
}

func filterJSONL(r io.Reader, format string, knownTypes map[string]bool) (FilteredTranscript, error) {
	result := FilteredTranscript{Format: format, NativeStartComplete: true}
	scanner := bufio.NewScanner(r)
	// Individual native JSONL records can contain tool output. A hard limit keeps
	// filtering bounded; exceeding it is refused rather than silently
	// truncated. The buffer holds a record plus its newline, so a record of
	// exactly maxRecordBytes is still read. bufio.Scanner allows the larger of
	// its maximum and the initial buffer's capacity, so the initial buffer
	// must not exceed the limit either.
	scanner.Buffer(make([]byte, min(64*1024, maxRecordBytes+1)), maxRecordBytes+1)
	lineNo, recognized := 0, 0
	gapSet := map[string]bool{}
	// Filter 2 collapsed every omission into one content-free gap, so a reader
	// could not see what this filter version was unable to keep. Collect the
	// distinct key names — names only, never values — and report them once.
	// Tool arguments dropped by the deny list are reported the same way under
	// their own code.
	var omittedKeys, deniedKeys keyNameSet
	addGap := func(code string, record int, detail string) {
		key := fmt.Sprintf("%s:%s", code, detail)
		if !gapSet[key] {
			gapSet[key] = true
			// Do not retain a source line number: an excluded preceding line must
			// not alter an otherwise identical retained snapshot.
			result.Gaps = append(result.Gaps, CaptureGap{Code: code, Detail: detail})
		}
	}
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		// bytes.TrimSpace, not strings.TrimSpace(string(line)): the same test
		// without copying a record that can be tens of megabytes.
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			result.NativeStartComplete = false
			addGap("incomplete_or_invalid_record", lineNo, "jsonl record omitted")
			continue
		}
		observed := parseNativeTimestamp(raw)
		if result.FirstEventAt.IsZero() {
			result.FirstEventAt = observed
		}
		if observed.IsZero() {
			if recordCarriesConversation(raw) {
				result.NativeStartComplete = false
			}
		} else if result.NativeStartAt.IsZero() || observed.Before(result.NativeStartAt) {
			result.NativeStartAt = observed
		}
		if !observed.IsZero() && (result.NativeEndAt.IsZero() || observed.After(result.NativeEndAt)) {
			result.NativeEndAt = observed
		}
		result.SessionIDs = appendUniqueString(result.SessionIDs, firstString(raw, "session_id", "sessionId"))
		result.AgentIDs = appendUniqueString(result.AgentIDs, firstString(raw, "agent_id", "agentId"))
		kind, _ := raw["type"].(string)
		if format == "claude-jsonl" && isCompactBoundary(raw) {
			recognized++
			encoded, err := json.Marshal(compactBoundaryRecord(raw, omittedKeys.add))
			if err != nil {
				return FilteredTranscript{}, &FilterError{Reason: "safe record cannot be encoded"}
			}
			result.Records = append(result.Records, encoded)
			result.Boundary.RetainedRecords++
			result.Boundary.RetainedBytes += len(encoded)
			continue
		}
		cursorRoleContent := format == "cursor-jsonl" && kind == "" && firstString(raw, "role") != ""
		if !knownTypes[kind] && !cursorRoleContent {
			addGap("unknown_record_type", lineNo, "record omitted")
			continue
		}
		recognized++
		if stripMetaRecordText(raw) {
			addGap("hidden_instruction_omitted", lineNo, "meta record text omitted")
		}
		state := sanitizeState{record: lineNo, addGap: addGap, omittedKey: omittedKeys.add, deniedKey: deniedKeys.add}
		safe, keep := sanitizeObject(raw, &state)
		if !keep {
			continue
		}
		encoded, err := json.Marshal(safe)
		if err != nil {
			return FilteredTranscript{}, &FilterError{Reason: "safe record cannot be encoded"}
		}
		result.Records = append(result.Records, encoded)
		result.Boundary.RetainedRecords++
		result.Boundary.RetainedBytes += len(encoded)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return FilteredTranscript{}, ErrRecordTooLarge
		}
		return FilteredTranscript{}, &FilterError{Reason: "transcript cannot be read"}
	}
	if lineNo > 0 && recognized == 0 {
		return FilteredTranscript{}, ErrUnsafeSourceFormat
	}
	if detail := omittedKeys.detail("omitted keys: "); detail != "" {
		addGap("unknown_field_omitted", 0, detail)
	}
	if detail := deniedKeys.detail(deniedToolArgumentIntro); detail != "" {
		addGap("sensitive_or_hidden_field_omitted", 0, detail)
	}
	sort.SliceStable(result.Gaps, func(i, j int) bool { return result.Gaps[i].Code < result.Gaps[j].Code })
	return result, nil
}

// isCompactBoundary reports whether a Claude Code record is the marker it
// writes where a conversation was compacted: exactly type "system" with
// subtype "compact_boundary". Every other system record stays hidden.
func isCompactBoundary(record map[string]any) bool {
	kind, _ := record["type"].(string)
	subtype, _ := record["subtype"].(string)
	return kind == "system" && subtype == "compact_boundary"
}

// compactBoundaryIDKeys are the identifiers a compact_boundary record keeps
// beside its type, subtype, and timestamp. logicalParentUuid is how Claude
// Code links the boundary to the last record before the compaction.
var compactBoundaryIDKeys = map[string]bool{"uuid": true, "parentUuid": true, "logicalParentUuid": true, "sessionId": true}

// compactBoundaryRecord rebuilds a compact_boundary record from what filter 5
// retains of it: type, subtype, its ids, its timestamp, and the isSidechain
// flag (so a subagent's compaction inlined in a parent transcript is excluded
// from the parent's counts like the rest of the subagent's records). Nothing
// else of a system record is kept, text above all; each omitted key's name is
// reported through omit. It is built from an allowlist of typed values rather
// than sanitized, because a system record is otherwise hidden whole.
func compactBoundaryRecord(raw map[string]any, omit func(string)) map[string]any {
	out := map[string]any{"type": "system", "subtype": "compact_boundary"}
	for key, value := range raw {
		switch {
		case key == "type" || key == "subtype":
		case key == "timestamp":
			if stamp, ok := value.(string); ok && !parseNativeTimestamp(map[string]any{"timestamp": stamp}).IsZero() {
				out[key] = stamp
			} else {
				omit(key)
			}
		case compactBoundaryIDKeys[key]:
			// An id is kept only when it looks like one (see
			// looksLikeRecordID). A null parent is kept as null.
			if value == nil {
				out[key] = nil
			} else if id, ok := value.(string); ok && looksLikeRecordID(id) {
				out[key] = id
			} else {
				omit(key)
			}
		case key == "isSidechain":
			if flag, ok := value.(bool); ok {
				out[key] = flag
			} else {
				omit(key)
			}
		default:
			omit(key)
		}
	}
	return out
}

// looksLikeRecordID reports whether a string can be kept as a record id on a
// rebuilt compact_boundary record: a short token with no whitespace and no
// tag brackets, which the credential redaction would leave unchanged. The
// boundary bypasses sanitizeObject, so this is what keeps its ids under the
// same value rules as every other retained string.
func looksLikeRecordID(value string) bool {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, " \t\r\n<>") {
		return false
	}
	_, sensitive := redactSensitive(value)
	return !sensitive
}

// isMetaRecord reports whether a native record is one Claude Code marked as
// harness-written with isMeta: true.
func isMetaRecord(record map[string]any) bool {
	flag, ok := record["isMeta"].(bool)
	return ok && flag
}

// stripMetaRecordText removes the text of an isMeta record before it is
// sanitized, and reports whether anything was removed. Such a record carries a
// harness-expanded skill or slash command, or a local-command caveat: injected
// instruction text, like a <system-reminder> block, not something a person
// wrote. The record itself, its ids, its parent link, and the isMeta flag are
// kept so the conversation's parent chain survives; string content and text
// blocks are removed at every depth of the content. Any other block keeps its
// shape and identifiers (a tool result keeps its tool_use_id and is_error),
// loses its own text and nested content text, and is then left to the
// ordinary rules. A message left with nothing but its role still keeps the
// record.
func stripMetaRecordText(record map[string]any) bool {
	if !isMetaRecord(record) {
		return false
	}
	stripped := stripContentText(record)
	if message, ok := record["message"].(map[string]any); ok {
		if stripContentText(message) {
			stripped = true
		}
		if len(message) == 0 {
			delete(record, "message")
		}
	}
	return stripped
}

// stripContentText removes string content and text blocks from one object's
// "content", dropping the key when nothing is left. A block that survives
// (a tool result, for instance) is stripped the same way: its own "text" and
// its nested "content" text go too, so no text of the harness-written record
// remains at any depth.
func stripContentText(holder map[string]any) bool {
	content, present := holder["content"]
	if !present {
		return false
	}
	switch value := content.(type) {
	case string:
		delete(holder, "content")
		return true
	case map[string]any:
		return stripBlockText(value)
	case []any:
		kept := make([]any, 0, len(value))
		stripped := false
		for _, raw := range value {
			switch block := raw.(type) {
			case string:
				stripped = true
			case map[string]any:
				if textBlockTypes[strings.ToLower(strings.TrimSpace(firstString(block, "type")))] {
					stripped = true
					continue
				}
				if stripBlockText(block) {
					stripped = true
				}
				kept = append(kept, block)
			default:
				kept = append(kept, raw)
			}
		}
		if len(kept) == 0 {
			delete(holder, "content")
		} else {
			holder["content"] = kept
		}
		return stripped
	}
	return false
}

// stripBlockText removes a non-text block's own "text" string and the text
// of its nested "content", recursively.
func stripBlockText(block map[string]any) bool {
	stripped := false
	if _, isText := block["text"].(string); isText {
		delete(block, "text")
		stripped = true
	}
	if stripContentText(block) {
		stripped = true
	}
	return stripped
}

func appendUniqueString(values []string, candidate string) []string {
	if candidate == "" {
		return values
	}
	for _, existing := range values {
		if existing == candidate {
			return values
		}
	}
	return append(values, candidate)
}

// conversationRecordTypes are the record types whose start time is part of a
// session's timestamp provenance. Harnesses also write bookkeeping entries
// beside the conversation — Claude Code's `summary` and `file-history-snapshot`
// records are the observed examples — which carry no top-level timestamp and
// no conversational content. Treating those as missing provenance would make
// an otherwise fully timestamped transcript permanently ineligible for child
// capture, so only conversation-bearing records are required to be stamped.
var conversationRecordTypes = map[string]bool{
	"user": true, "assistant": true, "system": true, "message": true,
	"tool_use": true, "tool_result": true, "tool_call": true,
	"session_meta": true, "turn_context": true, "response_item": true,
	"event_msg": true, "session": true, "event": true,
}

func recordCarriesConversation(record map[string]any) bool {
	if _, present := record["message"]; present {
		return true
	}
	if firstString(record, "role") != "" {
		return true
	}
	kind, _ := record["type"].(string)
	return conversationRecordTypes[strings.ToLower(strings.TrimSpace(kind))]
}

func parseNativeTimestamp(record map[string]any) time.Time {
	for _, key := range []string{"timestamp", "created_at"} {
		value, _ := record[key].(string)
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

type sanitizeState struct {
	record int
	addGap func(string, int, string)
	// extraAllowed widens the key allowlist for one archive-authored payload
	// shape. It applies at every depth of that payload, which is safe only
	// because such payloads are flat maps this repository writes itself.
	extraAllowed map[string]bool
	// retainAllKeys is set while sanitizing a tool-argument subtree, where the
	// argument names are the tool's own vocabulary and no allowlist can
	// anticipate them. Value sanitization is unchanged.
	retainAllKeys bool
	// numericOnly is set while sanitizing a token-accounting subtree.
	numericOnly bool
	// toolName is the name of the tool whose argument subtree is being
	// sanitized, read from the `name` or `tool_name` beside that subtree. It
	// decides whether typed-input arguments are denied.
	toolName string
	// omittedKey, when set, receives the name of each key the filter could not
	// keep so the caller can report the distinct names once. Without it an
	// omission falls back to the content-free unknown_field_omitted gap.
	omittedKey func(string)
	// deniedKey, when set, receives the name of each tool argument dropped by
	// the deny list. Without it the drop falls back to the content-free
	// sensitive_or_hidden_field_omitted gap.
	deniedKey func(string)
}

func (s *sanitizeState) omitField(key string) {
	if s.omittedKey != nil {
		s.omittedKey(key)
		return
	}
	s.addGap("unknown_field_omitted", s.record, "field omitted")
}

func (s *sanitizeState) denyArgument(key string) {
	if s.deniedKey != nil {
		s.deniedKey(key)
		return
	}
	s.addGap("sensitive_or_hidden_field_omitted", s.record, "field omitted")
}

func sanitizeObject(in map[string]any, state *sanitizeState) (map[string]any, bool) {
	if role, _ := in["role"].(string); isHiddenRole(role) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	if channel, _ := in["channel"].(string); isHiddenChannel(channel) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	if kind, _ := in["type"].(string); isHiddenRole(kind) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	// Filter 9: a pasted screenshot, a PDF, or an image a tool read arrives
	// as a content block whose data is base64. Its key names pass the
	// allowlist (type, source), so the block is recognized by shape and
	// dropped whole, wherever it sits, rather than kept key by key.
	if detail, binary := binaryContentBlock(in); binary && !state.numericOnly {
		state.addGap("binary_content_omitted", state.record, detail)
		return nil, false
	}
	out := make(map[string]any)
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := in[key]
		lower := strings.ToLower(key)
		if blockedKeys[lower] {
			state.addGap("sensitive_or_hidden_field_omitted", state.record, "field omitted")
			continue
		}
		switch {
		case state.numericOnly:
			if !isNumericSubtreeValue(value) {
				state.omitField(key)
				continue
			}
		case state.retainAllKeys:
			// A tool argument's own name is retained; its value is not trusted,
			// and a typed-input or credential-named argument is dropped whole.
			if deniedToolArgument(key, state.toolName) {
				state.denyArgument(key)
				continue
			}
		case !allowedKeys[lower] && !state.extraAllowed[lower]:
			state.omitField(key)
			continue
		case lower == "origin":
			// Filter 6 admits origin only as {kind: <string>}. Every other
			// member is omitted by name without being inspected further.
			kind, ok := originKindOnly(value, state)
			if !ok {
				state.omitField(key)
				continue
			}
			out[key] = kind
			continue
		case lower == "promptsource":
			if _, isString := value.(string); !isString {
				state.omitField(key)
				continue
			}
		case booleanFlagKeys[lower]:
			// Filter 4 admits isMeta, and filter 5 isCompactSummary and
			// isVisibleInTranscriptOnly, only as the boolean flag Claude Code
			// writes. Any other value under those names is prose the allowlist
			// never retained, and stays omitted.
			if _, isFlag := value.(bool); !isFlag {
				state.omitField(key)
				continue
			}
		}
		retainAll, numericOnly, toolName := state.retainAllKeys, state.numericOnly, state.toolName
		switch {
		case numericSubtreeKeys[lower]:
			state.numericOnly, state.retainAllKeys = true, false
		case toolArgumentKeys[lower] && !state.numericOnly:
			if !state.retainAllKeys {
				state.toolName = firstString(in, "name", "tool_name")
			}
			state.retainAllKeys = true
		}
		safe, keep := sanitizeValue(value, state)
		state.retainAllKeys, state.numericOnly, state.toolName = retainAll, numericOnly, toolName
		if keep {
			out[key] = safe
		}
	}
	if len(out) == 0 {
		state.addGap("record_without_allowed_fields_omitted", state.record, "record omitted")
		return nil, false
	}
	for _, key := range []string{"payload", "message", "item", "event"} {
		if _, had := in[key]; had {
			if _, kept := out[key]; !kept {
				state.addGap("hidden_or_unknown_nested_content_omitted", state.record, "record omitted")
				return nil, false
			}
		}
	}
	return out, true
}

// originKindOnly reduces a Claude Code origin object to {kind: <string>},
// reporting every other member as omitted. It keeps nothing when origin is not
// an object or has no string kind.
func originKindOnly(value any, state *sanitizeState) (map[string]any, bool) {
	origin, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range origin {
		if key != "kind" {
			state.omitField(key)
		}
	}
	kind, ok := origin["kind"].(string)
	if !ok || strings.TrimSpace(kind) == "" {
		return nil, false
	}
	safe, keep := sanitizeValue(kind, state)
	if !keep {
		return nil, false
	}
	return map[string]any{"kind": safe}, true
}

func isHiddenChannel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "analysis", "reasoning", "thinking", "chain_of_thought":
		return true
	}
	return false
}

// isNumericSubtreeValue reports whether a value may appear in a numbers-only
// subtree. Objects and arrays are admitted so the recursion can prune them;
// everything else, including strings and booleans, is omitted there.
func isNumericSubtreeValue(value any) bool {
	switch value.(type) {
	case float64, map[string]any, []any:
		return true
	default:
		return false
	}
}

func sanitizeValue(value any, state *sanitizeState) (any, bool) {
	switch v := value.(type) {
	case nil, bool, float64:
		return v, true
	case string:
		if injected, stripped := stripInjectedInstructions(v); injected {
			state.addGap("hidden_instruction_omitted", state.record, "injected instruction block omitted")
			if stripped == "" {
				return nil, false
			}
			v = stripped
		}
		if base64DataURL.MatchString(v) {
			state.addGap("binary_content_omitted", state.record, "base64 data URL omitted")
			v = base64DataURL.ReplaceAllString(v, "data:${1}${2};base64,[OMITTED]")
		}
		if redacted, hit := redactSensitive(v); hit {
			state.addGap("sensitive_content_redacted", state.record, "content redacted")
			v = redacted
		}
		const maxTextBytes = 64 * 1024
		if len(v) > maxTextBytes {
			// Cut on a character boundary, so a retained string stays valid
			// UTF-8 (filter 8 could split a multi-byte character).
			state.addGap("content_truncated", state.record, "content truncated")
			v = TruncateUTF8(v, maxTextBytes)
		}
		return v, true
	case map[string]any:
		return sanitizeObject(v, state)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			// An array inside a numbers-only subtree is filtered per element,
			// since only sanitizeObject sees the key that admitted it.
			if state.numericOnly && !isNumericSubtreeValue(item) {
				state.addGap("unsupported_value_omitted", state.record, "value omitted")
				continue
			}
			safe, keep := sanitizeValue(item, state)
			if keep {
				out = append(out, safe)
			}
		}
		if len(v) > 0 && len(out) == 0 {
			state.addGap("hidden_or_unknown_nested_content_omitted", state.record, "field omitted")
			return nil, false
		}
		return out, true
	default:
		state.addGap("unsupported_value_omitted", state.record, "value omitted")
		return nil, false
	}
}

func isHiddenRole(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "system", "developer", "reasoning", "analysis", "thinking", "chain_of_thought", "agent_reasoning", "agent_reasoning_delta", "raw_agent_reasoning":
		return true
	default:
		return false
	}
}

// IsFilterError supports callers which need to retain a previous source bundle
// when a new native format cannot be safely filtered.
func IsFilterError(err error) bool {
	var target *FilterError
	return errors.As(err, &target)
}
