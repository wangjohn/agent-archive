package archive

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
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

const adapterVersion = "0.3.0"

// maxOmittedKeyNames bounds how many distinct omitted key names one filtered
// transcript reports, so a pathological source cannot grow the gap list.
const maxOmittedKeyNames = 64

// DefaultParserVersion is the source parser version reported by this bounded
// foundation. The parser is intentionally partial until fixture coverage proves
// a given native format more completely.
const DefaultParserVersion = "0.6.0"

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

// CursorAdapter is intentionally limited to hook-provided JSONL records. Text
// paths and undocumented formats remain unsupported capture gaps upstream.
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
func (CursorAdapter) FilterText(r io.Reader, freshStartedAt time.Time) (FilteredTranscript, error) {
	if freshStartedAt.IsZero() {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no reliable fresh-session start"}
	}
	const maxText = 2 * 1024 * 1024
	content, err := io.ReadAll(io.LimitReader(r, maxText+1))
	if err != nil {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript cannot be read"}
	}
	if len(content) > maxText {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript exceeds safe size limit"}
	}
	result := FilteredTranscript{Format: "cursor-text", FirstEventAt: freshStartedAt.UTC(), Gaps: []CaptureGap{{Code: "text_structure_partial", Detail: "Cursor role sections retained without manufactured events"}}}
	var retained []string
	section := ""
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "system:") || strings.HasPrefix(lower, "developer:") || strings.HasPrefix(lower, "thinking:") || strings.HasPrefix(lower, "analysis:"):
			section = "hidden"
			result.Gaps = append(result.Gaps, CaptureGap{Code: "hidden_instruction_omitted", Detail: "text section omitted"})
		case strings.HasPrefix(lower, "user:") || strings.HasPrefix(lower, "assistant:") || strings.HasPrefix(lower, "tool:"):
			section = "visible"
			retained = append(retained, line)
		case section == "hidden":
			// continuation line of an already-hidden section; omit.
		case section == "visible":
			// continuation line of the current visible section's message body.
			retained = append(retained, line)
		default:
			return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has unrecognized role section"}
		}
	}
	if len(retained) == 0 {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable visible sections"}
	}
	state := sanitizeState{addGap: func(code string, _ int, detail string) {
		result.Gaps = append(result.Gaps, CaptureGap{Code: code, Detail: detail})
	}}
	safe, keep := sanitizeValue(strings.Join(retained, "\n"), &state)
	if !keep {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable content"}
	}
	text, ok := safe.(string)
	if !ok {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript is not text"}
	}
	result.Text = []string{text}
	result.Boundary.RetainedBytes = len(text)
	return result, nil
}

// injectedInstructionOpen matches the opening tag of a block a harness injects
// into an otherwise ordinary message: Claude Code wraps CLAUDE.md, hook output,
// and memory in <system-reminder>, and Codex writes AGENTS.md inside
// <user_instructions> and machine details inside <environment_context>. They
// are instructions to the model, not something the user wrote, so they are
// stripped from string content wherever they appear. Untagged instruction text
// is deliberately not guessed at.
var injectedInstructionOpen = regexp.MustCompile(`<(system-reminder|user_instructions|environment_context)\b[^>]*>`)

// injectedInstructionOpenByTag finds a further opening tag of one specific
// kind, so a block nested inside a block of the same kind extends the outer
// block instead of ending it early.
var injectedInstructionOpenByTag = map[string]*regexp.Regexp{
	"system-reminder":     regexp.MustCompile(`<system-reminder\b[^>]*>`),
	"user_instructions":   regexp.MustCompile(`<user_instructions\b[^>]*>`),
	"environment_context": regexp.MustCompile(`<environment_context\b[^>]*>`),
}

// stripInjectedInstructions removes every injected instruction block from one
// string. It reports whether anything was removed and returns the remaining
// text, which is empty when the string held nothing else. A block runs from
// its opening tag to the matching closing tag, counting nested blocks of the
// same kind; an opening tag whose block never closes, which a truncated or
// still-streaming record can produce, drops everything after it rather than
// retaining part of the block.
func stripInjectedInstructions(value string) (bool, string) {
	loc := injectedInstructionOpen.FindStringSubmatchIndex(value)
	if loc == nil {
		return false, value
	}
	var out strings.Builder
	rest := value
	for loc != nil {
		out.WriteString(rest[:loc[0]])
		end := injectedInstructionEnd(rest, loc[1], rest[loc[2]:loc[3]])
		if end < 0 {
			rest = ""
			break
		}
		rest = rest[end:]
		loc = injectedInstructionOpen.FindStringSubmatchIndex(rest)
	}
	out.WriteString(rest)
	return true, strings.TrimSpace(out.String())
}

// injectedInstructionEnd returns the index just past the closing tag which
// matches an opening tag of kind tag that ended at from, or -1 when the block
// never closes.
func injectedInstructionEnd(value string, from int, tag string) int {
	closeTag := "</" + tag + ">"
	open := injectedInstructionOpenByTag[tag]
	depth, pos := 1, from
	for depth > 0 {
		closeAt := strings.Index(value[pos:], closeTag)
		if closeAt < 0 {
			return -1
		}
		if openAt := open.FindStringIndex(value[pos:]); openAt != nil && openAt[0] < closeAt {
			depth++
			pos += openAt[1]
			continue
		}
		depth--
		pos += closeAt + len(closeTag)
	}
	return pos
}

// sensitiveValue matches credential assignments and the well-known key
// prefixes filter 2 already redacted. Its assignment form is deliberately
// broad and is a known false-positive class: ordinary code such as
// `token = parse(x)` matches it. See docs/agent-archive-privacy.md.
var sensitiveValue = regexp.MustCompile(`(?i)(?:\bauthorization\b\s*:\s*bearer\s+[^\s,;]+|\b(?:api[_-]?key|access[_-]?key|secret|password|authorization|bearer|token)\b\s*[=:]\s*[^\s,;]+|\bAKIA[0-9A-Z]{16}\b|\bsk-[A-Za-z0-9_-]{12,}\b)`)

// credentialShape matches credentials recognizable by their own structure
// rather than by an assignment around them: a PEM private key block (from
// its BEGIN line through the next END line, or to the end of the string when
// the END line is missing), a JWT (three base64url segments, the first
// beginning with `eyJ`), GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`,
// `ghr_`, `github_pat_`), and Slack tokens (`xox[baprs]-`). Anthropic and
// OpenAI style `sk-` keys are covered by sensitiveValue.
var credentialShape = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:.*?-----END [A-Z0-9 ]*-----|.*)|\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}|\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b|\bxox[baprs]-[A-Za-z0-9-]{10,}`)

// urlUserinfo matches the userinfo of a URL (`scheme://user:pass@host`, or a
// bare `scheme://user@host`). Only the userinfo is replaced; the scheme and
// host stay so the reference remains legible.
var urlUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@]+@`)

// redactSensitive applies every value-level credential pattern to one string
// and reports whether anything was replaced. The narrow structural patterns
// run before the broad assignment pattern so that, for example, a
// `x-access-token:…@host` URL keeps its host instead of losing everything
// after the word "token".
func redactSensitive(value string) (string, bool) {
	redacted := false
	if urlUserinfo.MatchString(value) {
		value = urlUserinfo.ReplaceAllString(value, "${1}[REDACTED]@")
		redacted = true
	}
	if credentialShape.MatchString(value) {
		value = credentialShape.ReplaceAllString(value, "[REDACTED]")
		redacted = true
	}
	if sensitiveValue.MatchString(value) {
		value = sensitiveValue.ReplaceAllString(value, "[REDACTED]")
		redacted = true
	}
	return value, redacted
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
	"status": true, "event_name": true, "turn_id": true, "reasoning_effort": true, "name": true, "items": true, "data": true,
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
}

// toolArgumentKeys name the subtrees which carry a tool call's own arguments.
// Filter 2 applied allowedKeys recursively inside them, which dropped every
// Edit old_string/new_string, Agent prompt, Grep pattern, and MCP argument and
// left tool evidence unusable. Inside these subtrees every argument name is
// retained; blockedKeys, sensitiveValue redaction, the string cap, and the
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
	// filtering bounded; exceeding it is unsafe rather than silently truncated.
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
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
		if len(strings.TrimSpace(string(line))) == 0 {
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
		cursorRoleContent := format == "cursor-jsonl" && kind == "" && firstString(raw, "role") != ""
		if !knownTypes[kind] && !cursorRoleContent {
			addGap("unknown_record_type", lineNo, "record omitted")
			continue
		}
		recognized++
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
		return FilteredTranscript{}, &FilterError{Reason: "record exceeds safe size limit or transcript cannot be read"}
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
		if redacted, hit := redactSensitive(v); hit {
			state.addGap("sensitive_content_redacted", state.record, "content redacted")
			v = redacted
		}
		const maxTextBytes = 64 * 1024
		if len(v) > maxTextBytes {
			state.addGap("content_truncated", state.record, "content truncated")
			v = v[:maxTextBytes]
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
