package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParseError means a parser could not derive a complete summary. The filtered
// source remains valid and should be retained with minimal failed metadata.
type ParseError struct{ Reason string }

func (e *ParseError) Error() string { return "metadata parse failed: " + e.Reason }

// NormalizedView is an in-memory analysis aid. It must never be uploaded as a
// second transcript representation.
type NormalizedView struct {
	Turns       []NormalizedTurn
	ToolCalls   []NormalizedToolCall
	ToolResults []NormalizedToolResult
	HookFinals  []HookFinalReconciliation
	// NativeSkillUses is collected in the same per-record walk as ToolCalls
	// (see toolActivity), rather than a second, separate traversal, so
	// deriveSkills only has to fold this in with supplemental evidence.
	NativeSkillUses []SkillUse
	// Tokens holds whatever token accounting the harness exposed in the
	// retained records. Absent accounting stays nil rather than zero.
	Tokens TokenUsage
	// CompactBoundaries and CompactSummaries count Claude Code's
	// compact_boundary records and isCompactSummary records.
	CompactBoundaries int
	CompactSummaries  int
}

// TokenUsage sums the token accounting a harness exposed. Each field is nil
// when no retained record reported it.
type TokenUsage struct {
	Input      *int `json:"input_tokens,omitempty"`
	Output     *int `json:"output_tokens,omitempty"`
	CacheRead  *int `json:"cache_read_tokens,omitempty"`
	CacheWrite *int `json:"cache_write_tokens,omitempty"`
}

// TurnKind classifies a visible record for counting. A harness writes tool
// results in the user role, so the role alone cannot say whether a human
// spoke.
type TurnKind string

const (
	// TurnKindHumanPrompt is a user record carrying text or any other
	// non-tool-result content: what a person actually sent.
	TurnKindHumanPrompt TurnKind = "human_prompt"
	TurnKindAssistant   TurnKind = "assistant"
	// TurnKindToolResult is a record whose content is only tool results.
	TurnKindToolResult TurnKind = "tool_result"
	// TurnKindHarnessMeta is a user record the harness wrote and marked
	// isMeta (an expanded skill or command). Filter 4 strips its text, so it
	// usually does not appear at all; a filter-3 bundle cannot tell it apart.
	TurnKindHarnessMeta TurnKind = "harness_meta"
	// TurnKindCommandOutput is the output of a local command or a `!` shell
	// command (<local-command-stdout>, <local-command-stderr>,
	// <local-command-caveat>, <bash-stdout>, <bash-stderr>).
	TurnKindCommandOutput TurnKind = "command_output"
	// TurnKindShellCommand is a `!` shell command the person ran directly
	// (<bash-input>). It counts as counts.user_shell_commands.
	TurnKindShellCommand TurnKind = "shell_command"
	// TurnKindLocalCommand is a typed slash command (<command-name>) which no
	// assistant answered before the next prompt, such as /model or /clear. A
	// slash command the assistant answered is a TurnKindHumanPrompt.
	TurnKindLocalCommand TurnKind = "local_command"
	// TurnKindCompactSummary is the user record Claude Code writes after
	// /compact or auto-compaction, marked isCompactSummary: a model-written
	// summary of the earlier conversation. Its text is kept, but it is
	// neither a prompt nor a message. Filters before 5 dropped the flag, so
	// in their bundles it still reads as a prompt.
	TurnKindCompactSummary TurnKind = "compact_summary"
)

// TurnModelSource names where a NormalizedTurn's model attribution came from.
type TurnModelSource string

const (
	TurnModelSourceTurnContext      TurnModelSource = "turn_context"
	TurnModelSourceNativeResponse   TurnModelSource = "native_response"
	TurnModelSourceNativeTranscript TurnModelSource = "native_transcript"
)

type NormalizedTurn struct {
	RecordIndex int      `json:"record_index"`
	Role        string   `json:"role"`
	Kind        TurnKind `json:"kind,omitempty"`
	// MessageID is the id of the API message a record belongs to (Claude's
	// message.id). Several streamed records can share one.
	MessageID     string          `json:"message_id,omitempty"`
	Text          string          `json:"text,omitempty"`
	Model         string          `json:"model,omitempty"`
	ResponseModel string          `json:"response_model,omitempty"`
	ModelSource   TurnModelSource `json:"model_source,omitempty"`
	Provider      string          `json:"provider,omitempty"`
	Reasoning     string          `json:"reasoning_level,omitempty"`
	ID            string          `json:"id,omitempty"`
	ParentID      string          `json:"parent_id,omitempty"`
	TurnID        string          `json:"turn_id,omitempty"`
	Timestamp     string          `json:"timestamp,omitempty"`
}

// HookFinalStatus reports how a hook-reported final response was reconciled
// against the native transcript's turns.
type HookFinalStatus string

const (
	HookFinalStatusUnreconciledIdentity HookFinalStatus = "unreconciled_identity"
	HookFinalStatusSeparateSubagent     HookFinalStatus = "separate_subagent"
	HookFinalStatusMatchedMessageID     HookFinalStatus = "matched_message_id"
	HookFinalStatusMatchedTurnID        HookFinalStatus = "matched_turn_id"
)

// HookFinalReconciliation keeps hook-only finals separate from native source.
// It never uses identical text as a deduplication signal.
type HookFinalReconciliation struct {
	EvidenceIndex int             `json:"evidence_index"`
	Status        HookFinalStatus `json:"status"`
	MessageID     string          `json:"message_id,omitempty"`
	TurnID        string          `json:"turn_id,omitempty"`
	AgentID       string          `json:"agent_id,omitempty"`
}

type NormalizedToolCall struct {
	RecordIndex int    `json:"record_index"`
	CallID      string `json:"call_id,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	Model       string `json:"model,omitempty"`
	Reasoning   string `json:"reasoning_level,omitempty"`
	Name        string `json:"name,omitempty"`
	// Input is the retained argument object exactly as the privacy filter
	// kept it. A harness which encodes its arguments as a JSON string (Codex)
	// contributes the decoded object; anything that does not decode to an
	// object stays nil.
	Input map[string]any `json:"input,omitempty"`
	// ResultRecordIndex, IsError, and OutputBytes describe the tool result
	// this call was linked to, and are nil when no result was observed.
	// OutputBytes measures the retained output, which the filter may have
	// redacted or capped.
	ResultRecordIndex *int  `json:"result_record_index,omitempty"`
	IsError           *bool `json:"is_error,omitempty"`
	OutputBytes       *int  `json:"output_bytes,omitempty"`
}

// NormalizedToolResult is a tool result observed in the source, before it is
// linked to the call it answers.
type NormalizedToolResult struct {
	RecordIndex int    `json:"record_index"`
	CallID      string `json:"call_id,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
	OutputBytes int    `json:"output_bytes"`
}

// ParseNormalized derives a narrow view from already-filtered source. The
// foundation recognizes visible user/assistant/tool messages only; all other
// retained native shapes stay available in SourceBundle for future parsers.
func ParseNormalized(bundle SourceBundle) (NormalizedView, error) {
	if err := validateBundle(bundle); err != nil {
		return NormalizedView{}, &ParseError{Reason: err.Error()}
	}
	view := NormalizedView{}
	isParentBundle := bundle.ParentSessionID == ""
	var codexModel, codexReasoning string
	var candidates []toolCallCandidate
	tokens := tokenTotals{}
	for i, record := range bundle.NativeRecords {
		if isParentBundle && isSidechainRecord(record) {
			// A subagent's records are archived as the child's own session.
			// Older Claude layouts inline them in the parent transcript; the
			// parent must not count the same messages, turns, and tool calls
			// a second time.
			continue
		}
		if bundle.Capture.Harness.Name == "codex" && firstString(record, "type") == "turn_context" {
			codexModel, codexReasoning = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort")
			continue
		}
		if isCompactBoundary(record) {
			// A marker only: filter 5 keeps its ids and timestamp, no text.
			view.CompactBoundaries++
			continue
		}
		accumulateTokens(record, &tokens)
		calls, results, skillUses := toolActivity(record, i, codexModel, codexReasoning)
		candidates = append(candidates, calls...)
		view.ToolResults = append(view.ToolResults, results...)
		view.NativeSkillUses = append(view.NativeSkillUses, skillUses...)
		role, text, kind, ok := visibleMessage(record)
		if !ok {
			continue
		}
		if isHiddenRole(role) {
			return NormalizedView{}, &ParseError{Reason: "hidden role present in filtered source"}
		}
		kind = refineUserKind(record, kind, text)
		if kind == TurnKindCompactSummary {
			view.CompactSummaries++
		}
		turn := NormalizedTurn{RecordIndex: i, Role: role, Kind: kind, MessageID: nestedMessageID(record), Text: text, Provider: firstStringDeep(record, "model_provider"), ID: firstStringDeep(record, "id", "uuid"), ParentID: firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"), TurnID: firstStringDeep(record, "turn_id"), Timestamp: firstStringDeep(record, "timestamp", "created_at")}
		if bundle.Capture.Harness.Name == "codex" {
			turn.Model, turn.Reasoning, turn.ModelSource = codexModel, codexReasoning, TurnModelSourceTurnContext
		} else if bundle.Capture.Harness.Name == "claude" {
			turn.ResponseModel, turn.ModelSource = firstStringDeep(record, "model", "model_id"), TurnModelSourceNativeResponse
		} else {
			turn.Model, turn.Reasoning, turn.ModelSource = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort"), TurnModelSourceNativeTranscript
		}
		view.Turns = append(view.Turns, turn)
	}
	resolveSlashCommands(view.Turns)
	view.ToolCalls = dedupeToolCalls(candidates)
	linkToolResults(view.ToolCalls, view.ToolResults)
	view.Tokens = tokens.usage()
	view.HookFinals = reconcileHookFinals(bundle, view.Turns)
	return view, nil
}

// harnessTextKinds classify a user record by the tag its text starts with.
// These records are written by the harness around something the person did,
// not typed as a prompt: Claude Code wraps a `!` shell command in
// <bash-input>, its output in <bash-stdout>/<bash-stderr>, a local command's
// output in <local-command-stdout>/<local-command-stderr>, the note it adds
// before local-command output in <local-command-caveat>, and a typed slash
// command in <command-name>/<command-message>/<command-args>.
var harnessTextKinds = []struct {
	tag  string
	kind TurnKind
}{
	{"bash-input", TurnKindShellCommand},
	{"bash-stdout", TurnKindCommandOutput},
	{"bash-stderr", TurnKindCommandOutput},
	{"local-command-stdout", TurnKindCommandOutput},
	{"local-command-stderr", TurnKindCommandOutput},
	{"local-command-caveat", TurnKindCommandOutput},
	{"command-name", TurnKindLocalCommand},
	{"command-message", TurnKindLocalCommand},
	{"command-args", TurnKindLocalCommand},
}

// refineUserKind reclassifies a record that looked like a human prompt but was
// written by the harness. A slash command is provisionally a local command;
// resolveSlashCommands promotes it to a prompt if the assistant answered it.
func refineUserKind(record map[string]any, kind TurnKind, text string) TurnKind {
	if kind != TurnKindHumanPrompt {
		return kind
	}
	if isCompactSummaryRecord(record) {
		return TurnKindCompactSummary
	}
	if isMetaRecord(record) {
		return TurnKindHarnessMeta
	}
	trimmed := strings.TrimSpace(text)
	for _, candidate := range harnessTextKinds {
		if strings.HasPrefix(trimmed, "<"+candidate.tag+">") {
			return candidate.kind
		}
	}
	return kind
}

// resolveSlashCommands decides which typed slash commands were prompts. A
// slash command that expands into a skill or custom command is answered by the
// assistant; a local one such as /model or /clear is answered only by
// local-command output. So a slash command counts as a prompt only if an
// assistant record follows before the next thing the person did, skipping
// harness-written records (isMeta expansions, command output, tool results).
// A compaction summary ends the scan without promoting: the conversation
// before it is summarized away, so an assistant record after it cannot be an
// answer to a command before it, and /compact itself is never a prompt.
func resolveSlashCommands(turns []NormalizedTurn) {
	for i := range turns {
		if turns[i].Kind != TurnKindLocalCommand {
			continue
		}
	scan:
		for _, next := range turns[i+1:] {
			switch next.Kind {
			case TurnKindAssistant:
				turns[i].Kind = TurnKindHumanPrompt
				break scan
			case TurnKindHumanPrompt, TurnKindLocalCommand, TurnKindShellCommand, TurnKindCompactSummary:
				break scan
			}
		}
	}
}

// isCompactSummaryRecord reports whether a Claude Code user record is the
// summary it writes after compaction. Only the boolean true that filter 5
// retains counts.
func isCompactSummaryRecord(record map[string]any) bool {
	flag, ok := record["isCompactSummary"].(bool)
	return ok && flag
}

// compactionsObservable reports whether a bundle can show how many times its
// session was compacted: only a Claude Code bundle from filter 5 on retains the
// compact_boundary record and the isCompactSummary flag. For anything else the
// count is unknown, not zero.
func compactionsObservable(bundle SourceBundle) bool {
	if canonicalHarnessName(bundle.Capture.Harness.Name) != "claude" {
		return false
	}
	version, err := strconv.Atoi(strings.TrimSpace(bundle.Capture.FilterVersion))
	return err == nil && version >= 5
}

func canonicalHarnessName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "claude-code" {
		return "claude"
	}
	return name
}

// nestedMessageID returns the id of the record's nested message object
// (Claude's message.id), which streamed records of one response share.
func nestedMessageID(record map[string]any) string {
	if message, ok := record["message"].(map[string]any); ok {
		return firstString(message, "id")
	}
	return ""
}

// isCodexStartupShell reports whether a completed item is Codex starting its
// own shell for the session rather than a command the model ran. Only a
// CommandExecution whose source is exactly "unified_exec_startup" qualifies;
// any other completed item, and a CommandExecution from any other source,
// is still a tool call.
func isCodexStartupShell(item map[string]any) bool {
	return strings.EqualFold(strings.TrimSpace(firstString(item, "type")), "commandexecution") && firstString(item, "source") == "unified_exec_startup"
}

// dedupeToolCalls drops the completion echo a harness writes for a call it
// already reported. Codex emits both a response_item for the call and a later
// item_completed for the same work; counting both would double every Codex
// tool call. An echo with no identity of its own is kept, since nothing proves
// it duplicates another record.
func dedupeToolCalls(candidates []toolCallCandidate) []NormalizedToolCall {
	reported := map[string]bool{}
	for _, candidate := range candidates {
		if !candidate.completionEcho && candidate.call.CallID != "" {
			reported[candidate.call.CallID] = true
		}
	}
	var out []NormalizedToolCall
	for _, candidate := range candidates {
		if candidate.completionEcho && candidate.call.CallID != "" && reported[candidate.call.CallID] {
			continue
		}
		out = append(out, candidate.call)
	}
	return out
}

// linkToolResults attaches each observed result to the call it answers: by
// tool_use_id for Claude, call_id for Codex, and by position for a harness
// which identifies neither (Cursor). Position is used only when no call in
// the bundle carries an identity at all; where the harness does identify its
// calls, a result naming one this bundle does not contain — or naming none —
// stays unlinked rather than being attached to the wrong call.
func linkToolResults(calls []NormalizedToolCall, results []NormalizedToolResult) {
	byCallID := map[string]int{}
	identified := false
	for index, call := range calls {
		if call.CallID == "" {
			continue
		}
		identified = true
		if _, seen := byCallID[call.CallID]; !seen {
			byCallID[call.CallID] = index
		}
	}
	linked := make([]bool, len(calls))
	attach := func(index int, result NormalizedToolResult) {
		recordIndex, isError, outputBytes := result.RecordIndex, result.IsError, result.OutputBytes
		calls[index].ResultRecordIndex = &recordIndex
		calls[index].IsError = &isError
		calls[index].OutputBytes = &outputBytes
		linked[index] = true
	}
	for _, result := range results {
		if result.CallID != "" {
			if index, found := byCallID[result.CallID]; found && !linked[index] {
				attach(index, result)
			}
			continue
		}
		if identified {
			continue
		}
		for index := range calls {
			if !linked[index] {
				attach(index, result)
				break
			}
		}
	}
}

func reconcileHookFinals(bundle SourceBundle, turns []NormalizedTurn) []HookFinalReconciliation {
	var out []HookFinalReconciliation
	for index, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != EvidenceKindFinalResponse {
			continue
		}
		final := HookFinalReconciliation{EvidenceIndex: index, MessageID: firstString(evidence.Payload, "message_id"), TurnID: firstString(evidence.Payload, "turn_id"), AgentID: firstString(evidence.Payload, "agent_id"), Status: HookFinalStatusUnreconciledIdentity}
		if final.AgentID != "" {
			final.Status = HookFinalStatusSeparateSubagent
		} else {
			for _, turn := range turns {
				if final.MessageID != "" && final.MessageID == turn.ID {
					final.Status = HookFinalStatusMatchedMessageID
					break
				}
				if final.TurnID != "" && turn.Role == "assistant" && final.TurnID == turn.TurnID {
					final.Status = HookFinalStatusMatchedTurnID
					break
				}
			}
		}
		out = append(out, final)
	}
	return out
}

// toolInvocationTypes name the shapes a harness uses to report that it called
// a tool: Claude's "tool_use", Codex's "function_call", "custom_tool_call" and
// "local_shell_call", and the generic "tool_call".
var toolInvocationTypes = map[string]bool{
	"tool_use": true, "tool_call": true, "function_call": true,
	"custom_tool_call": true, "local_shell_call": true,
}

// completedItemTypes name the Codex item.type values which report finished
// tool work inside an item_completed event.
var completedItemTypes = map[string]bool{
	"commandexecution": true, "mcptoolcall": true, "extension": true,
}

// toolResultTypes name the shapes which carry a tool's output back.
var toolResultTypes = map[string]bool{
	"tool_result": true, "function_call_output": true, "custom_tool_call_output": true,
	"local_shell_call_output": true,
}

// toolCallCandidate carries whether a call came from a completion event, which
// dedupeToolCalls needs and the published view does not.
type toolCallCandidate struct {
	call           NormalizedToolCall
	completionEcho bool
}

// toolActivity walks one native record once, extracting its normalized
// tool-call and tool-result entries together with any native skill
// invocation/read-inference signal found along the way — a single pass shared
// by ParseNormalized's ToolCalls and deriveSkills' native-record evidence,
// rather than each doing its own separate recursive walk over the same
// structure.
func toolActivity(record map[string]any, index int, model, reasoning string) ([]toolCallCandidate, []NormalizedToolResult, []SkillUse) {
	var calls []toolCallCandidate
	var results []NormalizedToolResult
	var skillUses []SkillUse
	parent := firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid")
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			kind := strings.ToLower(strings.TrimSpace(firstString(item, "type")))
			isToolInvocation := toolInvocationTypes[kind]
			tool := firstString(item, "name", "tool_name")
			arguments := toolArguments(item)
			if isToolInvocation {
				calls = append(calls, toolCallCandidate{call: NormalizedToolCall{
					RecordIndex: index, CallID: firstString(item, "call_id", "id"), ParentID: parent,
					Model: model, Reasoning: reasoning, Name: tool, Input: arguments,
				}})
			}
			if kind == "item_completed" {
				if completed, ok := item["item"].(map[string]any); ok && completedItemTypes[strings.ToLower(strings.TrimSpace(firstString(completed, "type")))] && !isCodexStartupShell(completed) {
					calls = append(calls, toolCallCandidate{completionEcho: true, call: NormalizedToolCall{
						RecordIndex: index, CallID: firstString(completed, "call_id", "id"), ParentID: parent,
						Model: model, Reasoning: reasoning, Name: firstString(completed, "name", "tool_name"),
						Input: toolArguments(completed),
					}})
				}
			}
			if toolResultTypes[kind] {
				isError, _ := item["is_error"].(bool)
				results = append(results, NormalizedToolResult{
					RecordIndex: index, CallID: firstString(item, "call_id", "tool_use_id"),
					IsError: isError, OutputBytes: len(toolResultOutput(item)),
				})
			}
			if isToolInvocation && strings.EqualFold(tool, "skill") {
				if name := firstString(arguments, "skill", "name"); name != "" {
					skillUses = append(skillUses, SkillUse{Name: name, Evidence: SkillUseEvidenceNativeInvocation})
				}
			}
			path := firstString(item, "file_path", "path")
			if path == "" {
				// A harness may carry its arguments as a nested object
				// (Claude's "input") or as a JSON-encoded string (Codex's
				// "arguments"); toolArguments has already decoded either.
				path = firstString(arguments, "file_path", "path")
			}
			command := firstString(item, "command", "arguments")
			readTool := strings.EqualFold(tool, "read") || strings.EqualFold(tool, "read_file")
			catRead := strings.HasPrefix(strings.TrimSpace(command), "cat ")
			if readTool || catRead {
				if name := skillNameFromPath(path); name != "" {
					skillUses = append(skillUses, SkillUse{Name: name, Evidence: SkillUseEvidenceReadInference})
				} else if catRead {
					if name := skillNameFromPath(command); name != "" {
						skillUses = append(skillUses, SkillUse{Name: name, Evidence: SkillUseEvidenceReadInference})
					}
				}
			}
			for _, child := range item {
				walk(child)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(record)
	return calls, results, skillUses
}

// toolArguments returns a tool call's retained arguments as an object. Claude
// nests them under "input"; Codex encodes them as a JSON string under
// "arguments" or "input". Anything which does not decode to an object yields
// nil rather than an invented shape.
func toolArguments(item map[string]any) map[string]any {
	for _, key := range []string{"input", "arguments", "tool_input"} {
		switch value := item[key].(type) {
		case map[string]any:
			return value
		case string:
			var decoded map[string]any
			if value != "" && json.Unmarshal([]byte(value), &decoded) == nil && len(decoded) > 0 {
				return decoded
			}
		}
	}
	return nil
}

// toolResultOutput returns the retained output text of one tool result, whose
// length is what OutputBytes reports.
func toolResultOutput(item map[string]any) string {
	if output, ok := item["output"].(string); ok {
		return output
	}
	if content, present := item["content"]; present {
		return contentText(content)
	}
	if result, ok := item["result"].(string); ok {
		return result
	}
	return ""
}

// isSidechainRecord reports whether a native record belongs to a subagent
// rather than to the session that owns the transcript. Only an explicit true
// counts: an absent or non-boolean flag leaves the record in place.
func isSidechainRecord(record map[string]any) bool {
	for _, key := range []string{"isSidechain", "is_sidechain"} {
		if flag, ok := record[key].(bool); ok && flag {
			return true
		}
	}
	return false
}

// visibleMessage finds the record's authored message and classifies it. A
// Cursor record carries its role at the top level and its content under
// "message", which the filter-2 era parser never looked for, so every Cursor
// session derived zero turns.
func visibleMessage(record map[string]any) (string, string, TurnKind, bool) {
	if role, _ := record["role"].(string); role != "" {
		content, present := record["content"]
		if !present {
			if nested, ok := record["message"].(map[string]any); ok {
				content, present = nested["content"]
			}
		}
		if present {
			if text, kind, ok := classifyContent(role, content); ok {
				return role, text, kind, true
			}
		}
		if role == "tool" {
			return role, "", TurnKindToolResult, true
		}
	}
	for _, key := range []string{"message", "payload", "item", "event"} {
		if nested, ok := record[key].(map[string]any); ok {
			if role, text, kind, found := visibleMessage(nested); found {
				return role, text, kind, true
			}
		}
	}
	return "", "", "", false
}

// classifyContent separates what a person sent from what a harness wrote back
// in the user role. A user record whose content is only tool results is not a
// prompt, and counting it as one is what made a four-prompt session report
// twenty-eight turns.
func classifyContent(role string, content any) (string, TurnKind, bool) {
	text, toolResults, other := visibleContent(content)
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "assistant":
		return text, TurnKindAssistant, true
	case "tool":
		return text, TurnKindToolResult, true
	}
	if text != "" || other > 0 {
		return text, TurnKindHumanPrompt, true
	}
	if toolResults > 0 {
		return text, TurnKindToolResult, true
	}
	// A record with a role but nothing retained is not a visible message.
	return "", "", false
}

// textBlockTypes name the content blocks whose only payload is text. When the
// filter strips such a block's text — an injected <system-reminder> or
// <user_instructions> block is the common case — the block survives as a bare
// `{type: "text"}` that carries nothing a person sent.
var textBlockTypes = map[string]bool{"text": true, "input_text": true, "output_text": true, "": true}

// visibleContent reports a message's text, how many tool results it carries,
// and how many other blocks it carries. Tool-result text is deliberately not
// folded into the message text: it is the tool speaking, not the author. A
// text block left empty by the filter counts as nothing at all; otherwise a
// tool-result record with a stripped reminder beside it, or a prompt that was
// only injected instructions, would still count as a human prompt.
func visibleContent(content any) (string, int, int) {
	switch value := content.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return "", 0, 0
		}
		return value, 0, 1
	case map[string]any:
		return visibleContent([]any{value})
	case []any:
		parts := make([]string, 0, len(value))
		toolResults, other := 0, 0
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				if text := contentText(raw); text != "" {
					parts = append(parts, text)
					other++
				}
				continue
			}
			kind := strings.ToLower(strings.TrimSpace(firstString(block, "type")))
			switch {
			case toolResultTypes[kind]:
				toolResults++
			case toolInvocationTypes[kind]:
				other++
			default:
				text := contentText(block)
				if text == "" && textBlockTypes[kind] {
					continue
				}
				if text != "" {
					parts = append(parts, text)
				}
				other++
			}
		}
		return strings.Join(parts, "\n"), toolResults, other
	}
	return "", 0, 0
}

func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := contentText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, _ := v["text"].(string); text != "" {
			return text
		}
		if content, ok := v["content"]; ok {
			return contentText(content)
		}
	}
	return ""
}

func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, _ := record[key].(string); value != "" {
			return value
		}
	}
	return ""
}

// tokenTotals collects whatever token accounting the retained records expose,
// keyed by the message the accounting belongs to. Claude Code writes one JSONL
// record per content block of a single API message, and every one of those
// records repeats the same `message.id` and the same `message.usage`; adding
// each record's usage would count one response as many. The latest record seen
// for a message id replaces the earlier ones. Accounting with no message
// identity (Codex's `turn_token_usage`) is summed as it comes.
type tokenTotals struct {
	byMessage map[string]map[string]any
	anonymous []map[string]any
}

func (t *tokenTotals) observe(usage map[string]any, messageID string) {
	if messageID == "" {
		t.anonymous = append(t.anonymous, usage)
		return
	}
	if t.byMessage == nil {
		t.byMessage = map[string]map[string]any{}
	}
	t.byMessage[messageID] = usage
}

// usage sums the collected accounting. A field stays nil until some record
// reports it, so "no accounting" is never published as zero tokens.
func (t tokenTotals) usage() TokenUsage {
	var out TokenUsage
	add := func(target **int, source map[string]any, keys ...string) {
		for _, key := range keys {
			value, ok := source[key].(float64)
			if !ok {
				continue
			}
			total := int(value)
			if *target != nil {
				total += **target
			}
			*target = &total
			return
		}
	}
	sources := append([]map[string]any(nil), t.anonymous...)
	for _, source := range t.byMessage {
		sources = append(sources, source)
	}
	for _, source := range sources {
		add(&out.Input, source, "input_tokens", "prompt_tokens")
		add(&out.Output, source, "output_tokens", "completion_tokens")
		add(&out.CacheRead, source, "cache_read_input_tokens", "cached_input_tokens")
		add(&out.CacheWrite, source, "cache_creation_input_tokens")
	}
	return out
}

// accumulateTokens records one record's token accounting: Claude stamps
// `message.usage` on each assistant record and Codex writes `turn_token_usage`
// on each token_usage_record. Cumulative (`total_token_usage`) and thread-wide
// figures are deliberately ignored, so the sum stays additive across records.
// The accounting is attributed to the `id` of the object that carries it
// (Claude's `message.id`), which is what lets repeated streamed records of one
// message count once.
func accumulateTokens(record map[string]any, totals *tokenTotals) {
	usage, owner := firstMapDeepOwner(record, "usage")
	if usage == nil {
		usage, owner = firstMapDeepOwner(record, "turn_token_usage")
	}
	if usage == nil {
		return
	}
	totals.observe(usage, firstString(owner, "id"))
}

// nativeTurnEnd reads Cursor's own end-of-turn record. Cursor has no lifecycle
// hook for this, so without it a Cursor session's outcome stays unknown even
// though the transcript states it.
func nativeTurnEnd(bundle SourceBundle) (MetadataState, TurnOutcome, bool) {
	if bundle.Capture.Harness.Name != "cursor" {
		return "", "", false
	}
	state, outcome, found := MetadataStateUnknown, TurnOutcomeUnknown, false
	for _, record := range bundle.NativeRecords {
		if strings.ToLower(strings.TrimSpace(firstString(record, "type"))) != "turn_ended" {
			continue
		}
		// The turn is over whatever it reported; a later record wins.
		state, found = MetadataStateIdle, true
		switch strings.ToLower(strings.TrimSpace(firstString(record, "status"))) {
		case "completed":
			outcome = TurnOutcomeCompleted
		case "aborted", "cancelled", "canceled", "interrupted":
			outcome = TurnOutcomeInterrupted
		case "error", "failed":
			outcome = TurnOutcomeError
		default:
			outcome = TurnOutcomeUnknown
		}
	}
	return state, outcome, found
}

// firstMapDeepOwner finds the first object stored under key, searching the
// record and then its nested message/payload/item/event objects, and also
// returns the object that holds it.
func firstMapDeepOwner(record map[string]any, key string) (map[string]any, map[string]any) {
	if value, ok := record[key].(map[string]any); ok {
		return value, record
	}
	for _, nested := range []string{"message", "payload", "item", "event"} {
		if child, ok := record[nested].(map[string]any); ok {
			if value, owner := firstMapDeepOwner(child, key); value != nil {
				return value, owner
			}
		}
	}
	return nil, nil
}

func firstStringDeep(record map[string]any, keys ...string) string {
	if value := firstString(record, keys...); value != "" {
		return value
	}
	for _, key := range []string{"message", "payload", "item", "event"} {
		if nested, ok := record[key].(map[string]any); ok {
			if value := firstStringDeep(nested, keys...); value != "" {
				return value
			}
		}
	}
	return ""
}

// BuildMetadata derives a replaceable metadata sidecar. If parsing fails, it
// returns failed minimal metadata together with a ParseError; callers should
// still publish the verified filtered source and retry a parser upgrade later.
func BuildMetadata(bundle SourceBundle, machineID string, startedAt, derivedAt time.Time, reference SourceReference, parser ParserInfo) (Metadata, error) {
	if err := validateBundle(bundle); err != nil {
		return Metadata{}, err
	}
	if strings.TrimSpace(machineID) == "" || startedAt.IsZero() || derivedAt.IsZero() {
		return Metadata{}, errors.New("machine ID, start time, and derivation time are required")
	}
	if strings.TrimSpace(reference.Key) == "" || len(reference.SHA256) != 64 || reference.CompressedBytes < 0 {
		return Metadata{}, errors.New("verified source reference is required")
	}
	if parser.Name == "" {
		parser.Name = bundle.Capture.AdapterName
	}
	if parser.Version == "" {
		parser.Version = DefaultParserVersion
	}
	if parser.Status == "" {
		parser.Status = ParserStatusPartial
	}
	metadata := Metadata{
		SchemaVersion: MetadataSchemaVersion, SessionID: bundle.ArchiveSessionID, NativeSessionID: bundle.NativeSessionID,
		MachineID: machineID, ProjectID: bundle.ProjectID, StartedAt: startedAt.UTC(), CapturedAt: bundle.Capture.CapturedAt.UTC(),
		MetadataDerivedAt: derivedAt.UTC(), Harness: bundle.Capture.Harness,
		Adapter: AdapterInfo{Name: bundle.Capture.AdapterName, Version: bundle.Capture.AdapterVersion}, Parser: parser,
		FilterVersion: bundle.Capture.FilterVersion, State: MetadataStateUnknown, TurnOutcome: TurnOutcomeUnknown,
		SemanticConventions: &SemanticConventionsInfo{Name: "OpenTelemetry GenAI semantic conventions", Revision: OpenTelemetryGenAIRevision},
		SkillDetection:      SkillDetectionUnavailable,
		CaptureGaps:         append([]CaptureGap(nil), bundle.Capture.Gaps...), SourceBundle: reference,
		ParentSessionID: bundle.ParentSessionID,
		LinkedSessions:  append([]LinkedSessionReference(nil), bundle.LinkedSessions...),
	}
	metadata.State, metadata.TurnOutcome = deriveLifecycle(bundle.SupplementalEvidence)
	view, err := ParseNormalized(bundle)
	if err != nil {
		metadata.Parser.Status = ParserStatusFailed
		return metadata, err
	}
	// A native end-of-turn record fills in only what the hook evidence could
	// not establish: an observed hook stop, interrupt, or closure still wins.
	if state, outcome, found := nativeTurnEnd(bundle); found {
		if metadata.State == MetadataStateUnknown {
			metadata.State = state
		}
		if metadata.TurnOutcome == TurnOutcomeUnknown {
			metadata.TurnOutcome = outcome
		}
	}
	prompts, messages, shellCommands := 0, 0, 0
	assistantMessages := map[string]bool{}
	models := map[string]*ModelSummary{}
	for _, turn := range view.Turns {
		switch turn.Kind {
		case TurnKindHumanPrompt:
			prompts++
			messages++
		case TurnKindAssistant:
			// One streamed response is several records sharing message.id:
			// it is one message, and one turn for its model.
			if turn.MessageID != "" {
				if assistantMessages[turn.MessageID] {
					continue
				}
				assistantMessages[turn.MessageID] = true
			}
			messages++
		case TurnKindShellCommand:
			shellCommands++
			continue
		default:
			// A tool result, command output, an unanswered slash command, or a
			// harness-written record is not a message any author sent.
			continue
		}
		if turn.Role != "user" && turn.Role != "assistant" {
			continue
		}
		modelName, attribute, responseStatus := turn.Model, "gen_ai.request.model", ResponseModelStatusNotExposed
		if modelName == "" && turn.ResponseModel != "" {
			modelName, attribute, responseStatus = turn.ResponseModel, "gen_ai.response.model", ResponseModelStatusObserved
		}
		if modelName == "" {
			continue
		}
		key := attribute + "\x00" + turn.Provider + "\x00" + modelName + "\x00" + turn.Reasoning
		model := models[key]
		if model == nil {
			attributes := map[string]string{attribute: modelName}
			if turn.Provider != "" {
				attributes["gen_ai.provider.name"] = turn.Provider
			}
			if turn.Reasoning != "" {
				attributes["agent_archive.request.reasoning_level"] = turn.Reasoning
			}
			model = &ModelSummary{Attributes: attributes, Source: ModelSummarySourceNativeTranscript, ResponseModelStatus: responseStatus}
			models[key] = model
		}
		if model.TurnCount == nil {
			zero := 0
			model.TurnCount = &zero
		}
		*model.TurnCount++
	}
	// Native text is retained precisely because its structure is not proven.
	// Counts derived only from the structured subset would look complete, so
	// leave all structure-dependent totals unknown whenever text is present.
	if len(bundle.NativeText) == 0 {
		toolCalls, toolResults := len(view.ToolCalls), len(view.ToolResults)
		metadata.Counts.Turns = &prompts
		metadata.Counts.Messages = &messages
		metadata.Counts.ToolCalls = &toolCalls
		metadata.Counts.ToolResults = &toolResults
		metadata.Counts.UserShellCommands = &shellCommands
		metadata.Counts.InputTokens, metadata.Counts.OutputTokens = view.Tokens.Input, view.Tokens.Output
		metadata.Counts.CacheReadTokens, metadata.Counts.CacheWriteTokens = view.Tokens.CacheRead, view.Tokens.CacheWrite
		if compactionsObservable(bundle) {
			// Each compaction writes one boundary and one summary. Count
			// boundaries; fall back to summaries only when no boundary was
			// retained at all, so the same compaction is never counted twice.
			compactions := view.CompactBoundaries
			if compactions == 0 {
				compactions = view.CompactSummaries
			}
			metadata.Counts.Compactions = &compactions
		}
	}
	modelKeys := make([]string, 0, len(models))
	for key := range models {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		metadata.Models = append(metadata.Models, *models[key])
	}
	deriveHookModels(bundle, &metadata)
	deriveSkills(bundle, view.NativeSkillUses, &metadata)
	feedback := 0
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind == EvidenceKindExplicitFeedback {
			feedback++
		}
	}
	metadata.Counts.ExplicitFeedback = &feedback
	return metadata, nil
}

type lifecycleObservation struct {
	at         time.Time
	event      string
	status     string
	provenance string
	payloadKey string
}

// deriveLifecycle applies conservative rules to filtered hook evidence.
// Sorting makes duplicate, delayed, and out-of-order delivery deterministic.
// Evidence captured before event_name was retained stays unknown rather than
// being reconstructed from local operational request state.
func deriveLifecycle(evidence []SupplementalEvidence) (MetadataState, TurnOutcome) {
	var observations []lifecycleObservation
	for _, item := range evidence {
		if item.Kind != EvidenceKindLifecycleHook {
			continue
		}
		event := strings.ToLower(strings.ReplaceAll(firstString(item.Payload, "event_name"), "_", ""))
		if event == "" {
			continue
		}
		payload, _ := json.Marshal(item.Payload)
		observations = append(observations, lifecycleObservation{
			at: item.ObservedAt, event: event, status: strings.ToLower(firstString(item.Payload, "status")),
			provenance: item.Provenance, payloadKey: string(payload),
		})
	}
	sort.Slice(observations, func(i, j int) bool {
		if !observations[i].at.Equal(observations[j].at) {
			return observations[i].at.Before(observations[j].at)
		}
		if lifecycleRank(observations[i].event) != lifecycleRank(observations[j].event) {
			return lifecycleRank(observations[i].event) < lifecycleRank(observations[j].event)
		}
		left := observations[i].event + "\x00" + observations[i].status + "\x00" + observations[i].provenance + "\x00" + observations[i].payloadKey
		right := observations[j].event + "\x00" + observations[j].status + "\x00" + observations[j].provenance + "\x00" + observations[j].payloadKey
		return left < right
	})
	state, outcome := MetadataStateUnknown, TurnOutcomeUnknown
	for _, observation := range observations {
		switch observation.event {
		case "sessionstart", "userpromptsubmit", "beforesubmitprompt":
			state, outcome = MetadataStateActive, TurnOutcomeUnknown
		case "stop":
			state, outcome = MetadataStateIdle, documentedLifecycleOutcome(observation.event, observation.status, observation.provenance)
		case "interrupt":
			state, outcome = MetadataStateIdle, TurnOutcomeInterrupted
		case "stopfailure":
			state, outcome = MetadataStateIdle, TurnOutcomeError
		case "sessionend":
			state = MetadataStateClosed
			// Closing the session does not undo a previously observed turn outcome.
			if observed := documentedLifecycleOutcome(observation.event, observation.status, observation.provenance); observed != TurnOutcomeUnknown {
				outcome = observed
			}
		}
		// A subagent finishing says nothing about the parent session, which
		// is still running: its state and outcome are left as observed.
	}
	return state, outcome
}

func lifecycleRank(event string) int {
	switch event {
	case "sessionstart", "userpromptsubmit", "beforesubmitprompt":
		return 1
	case "stop", "interrupt", "stopfailure":
		return 2
	case "sessionend", "subagentstop":
		return 3
	default:
		return 0
	}
}

func documentedLifecycleOutcome(event, status, provenance string) TurnOutcome {
	if event == "stop" && provenance != "hook:cursor:stop" {
		return TurnOutcomeUnknown
	}
	if event == "sessionend" && provenance != "hook:cursor:sessionend" {
		return TurnOutcomeUnknown
	}
	switch status {
	case "completed":
		return TurnOutcomeCompleted
	case "aborted":
		return TurnOutcomeInterrupted
	case "error":
		return TurnOutcomeError
	default:
		return TurnOutcomeUnknown
	}
}

func deriveHookModels(bundle SourceBundle, metadata *Metadata) {
	seen := map[string]bool{}
	for _, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != EvidenceKindLifecycleHook && evidence.Kind != EvidenceKindFinalResponse {
			continue
		}
		id, label := firstString(evidence.Payload, "model_id"), firstString(evidence.Payload, "model")
		if id == "" {
			continue
		}
		key := id + "\x00" + label
		if seen[key] {
			continue
		}
		seen[key] = true
		attrs := map[string]string{"gen_ai.request.model": id}
		if label != "" {
			attrs["agent_archive.request.model_label"] = label
		}
		if params, ok := evidence.Payload["model_params"].([]any); ok {
			for _, raw := range params {
				if p, ok := raw.(map[string]any); ok {
					if name, value := firstString(p, "id"), firstString(p, "value"); name != "" {
						attrs["agent_archive.request.setting."+name] = value
					}
				}
			}
		}
		metadata.Models = append(metadata.Models, ModelSummary{Attributes: attrs, Source: ModelSummarySourceHook, ResponseModelStatus: ResponseModelStatusNotExposed})
	}
}

func deriveSkills(bundle SourceBundle, nativeSkillUses []SkillUse, metadata *Metadata) {
	available := map[string]SkillSnapshot{}
	used := map[string]SkillUse{}
	recordUse := func(entry SkillUse) {
		if entry.SHA256 == "" {
			for _, existing := range used {
				if existing.Name == entry.Name && existing.SHA256 != "" {
					return
				}
			}
		} else {
			delete(used, entry.Name+"\x00")
		}
		key := entry.Name + "\x00" + entry.SHA256
		if existing, ok := used[key]; ok && existing.Evidence == SkillUseEvidenceNativeInvocation {
			return
		}
		used[key] = entry
	}
	for _, evidence := range bundle.SupplementalEvidence {
		name := firstString(evidence.Payload, "name")
		if name == "" && evidence.Kind != EvidenceKindSkillInventory {
			continue
		}
		hash := firstString(evidence.Payload, "sha256")
		switch evidence.Kind {
		case EvidenceKindSkillInventory, EvidenceKindSkillDiscovered, EvidenceKindSkillSnapshot:
			coverage := SkillCoverage(firstString(evidence.Payload, "coverage"))
			if skills, ok := evidence.Payload["skills"].([]any); ok {
				for _, raw := range skills {
					if skill, ok := raw.(map[string]any); ok {
						n, h := firstString(skill, "name"), firstString(skill, "sha256")
						if n != "" {
							available[n+"\x00"+h] = SkillSnapshot{Name: n, SHA256: h, Coverage: coverage}
						}
					}
				}
			} else {
				available[name+"\x00"+hash] = SkillSnapshot{Name: name, SHA256: hash, Coverage: coverage}
			}
		case EvidenceKindSkillInvocation:
			recordUse(SkillUse{Name: name, SHA256: hash, Evidence: SkillUseEvidenceNativeInvocation})
		case EvidenceKindSkillRead:
			recordUse(SkillUse{Name: name, SHA256: hash, Evidence: SkillUseEvidenceReadInference})
		}
	}
	// Native invocation/read-inference evidence is collected once, in
	// toolCalls()'s per-record walk (see ParseNormalized), rather than a
	// second traversal of bundle.NativeRecords here.
	for _, entry := range nativeSkillUses {
		recordUse(entry)
	}
	for _, entry := range available {
		metadata.SkillsAvailable = append(metadata.SkillsAvailable, entry)
	}
	for _, entry := range used {
		metadata.SkillsUsed = append(metadata.SkillsUsed, entry)
	}
	if len(metadata.SkillsUsed) > 0 {
		metadata.SkillDetection = SkillDetectionObserved
	} else {
		for _, entry := range metadata.SkillsAvailable {
			if entry.Coverage == SkillCoverageEligible || entry.Coverage == SkillCoverageDiscovered {
				metadata.SkillDetection = SkillDetectionPartial
				break
			}
		}
	}
	sort.Slice(metadata.SkillsAvailable, func(i, j int) bool {
		return metadata.SkillsAvailable[i].Name+"\x00"+metadata.SkillsAvailable[i].SHA256 < metadata.SkillsAvailable[j].Name+"\x00"+metadata.SkillsAvailable[j].SHA256
	})
	sort.Slice(metadata.SkillsUsed, func(i, j int) bool {
		return metadata.SkillsUsed[i].Name+"\x00"+metadata.SkillsUsed[i].SHA256 < metadata.SkillsUsed[j].Name+"\x00"+metadata.SkillsUsed[j].SHA256
	})
}

func skillNameFromPath(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	marker := "/SKILL.md"
	index := strings.Index(value, marker)
	if index < 1 {
		return ""
	}
	before := strings.TrimSuffix(value[:index], "/")
	parts := strings.Split(before, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return ""
	}
	return parts[len(parts)-1]
}

// IsParseError supports the source-first publication flow.
func IsParseError(err error) bool {
	var target *ParseError
	return errors.As(err, &target)
}

func (m Metadata) ValidateSourceReference() error {
	if m.SchemaVersion != MetadataSchemaVersion {
		return fmt.Errorf("unsupported metadata schema version %d", m.SchemaVersion)
	}
	if m.SourceBundle.Key == "" || len(m.SourceBundle.SHA256) != 64 {
		return errors.New("metadata has no verified source reference")
	}
	return nil
}
