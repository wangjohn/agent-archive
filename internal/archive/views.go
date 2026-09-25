package archive

import (
	"errors"
	"strconv"
	"strings"
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
	// TurnKindAssistant is a record in the assistant role.
	TurnKindAssistant TurnKind = "assistant"
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
	// TurnKindHarnessNotification is a user record the harness produced on
	// its own, such as Claude Code's background-task completion notice
	// (origin.kind "task-notification"). Filter 6 retains origin; an older
	// bundle cannot tell such a record from a prompt.
	TurnKindHarnessNotification TurnKind = "harness_notification"
)

// TurnModelSource names where a NormalizedTurn's model attribution came from.
type TurnModelSource string

// Turn model sources, one per harness.
const (
	// TurnModelSourceTurnContext is Codex: the model and reasoning level of
	// the latest turn_context record, as requested.
	TurnModelSourceTurnContext TurnModelSource = "turn_context"
	// TurnModelSourceNativeResponse is Claude Code: the model named on the
	// response message itself (NormalizedTurn.ResponseModel).
	TurnModelSourceNativeResponse TurnModelSource = "native_response"
	// TurnModelSourceNativeTranscript is any other harness: a model field on
	// the record, treated as the requested model.
	TurnModelSourceNativeTranscript TurnModelSource = "native_transcript"
)

// NormalizedTurn is one visible message record of a filtered source in a
// harness-independent shape: its role and TurnKind, retained
// text, model attribution, and the IDs and timestamp the record carried.
// RecordIndex is its position in SourceBundle.NativeRecords.
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

// Hook final statuses.
const (
	// HookFinalStatusUnreconciledIdentity means no transcript turn carried
	// the final response's message or turn ID.
	HookFinalStatusUnreconciledIdentity HookFinalStatus = "unreconciled_identity"
	// HookFinalStatusSeparateSubagent means the final response came from a
	// subagent (it names an agent ID), so it is not matched to this session's
	// turns.
	HookFinalStatusSeparateSubagent HookFinalStatus = "separate_subagent"
	// HookFinalStatusMatchedMessageID means a turn has the final response's
	// message ID.
	HookFinalStatusMatchedMessageID HookFinalStatus = "matched_message_id"
	// HookFinalStatusMatchedTurnID means an assistant turn has the final
	// response's turn ID.
	HookFinalStatusMatchedTurnID HookFinalStatus = "matched_turn_id"
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

// NormalizedToolCall is one tool call found in a filtered source, with the
// result linked to it when one was observed. RecordIndex is the position of
// the record that holds it in SourceBundle.NativeRecords.
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

	// raw is the retained native object this call was read from, and
	// resultText the retained output of the result linked to it. They are
	// kept from the single toolActivity walk for BuildHandoff, which needs a
	// custom tool's raw string input and the result text; neither is part of
	// the published view.
	raw        map[string]any
	resultText string
}

// NormalizedToolResult is a tool result observed in the source, before it is
// linked to the call it answers.
type NormalizedToolResult struct {
	RecordIndex int    `json:"record_index"`
	CallID      string `json:"call_id,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
	OutputBytes int    `json:"output_bytes"`

	// text is the retained output OutputBytes measures; see
	// NormalizedToolCall.resultText.
	text string
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
		if bundle.harness() == "codex" && firstString(record, "type") == "turn_context" {
			codexModel, codexReasoning = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort")
			continue
		}
		if isCompactBoundary(record) {
			// A marker only: filter 5 keeps its ids and timestamp, no text.
			view.CompactBoundaries++
			continue
		}
		if !isPlaceholderModel(firstStringDeep(record, "model", "model_id")) {
			accumulateTokens(record, &tokens)
		}
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
		var model, responseModel, reasoning string
		var modelSource TurnModelSource
		switch bundle.harness() {
		case "codex":
			model, reasoning, modelSource = codexModel, codexReasoning, TurnModelSourceTurnContext
		case "claude":
			responseModel, modelSource = recordModel(record), TurnModelSourceNativeResponse
		default:
			model, reasoning, modelSource = recordModel(record), firstStringDeep(record, "reasoning_effort"), TurnModelSourceNativeTranscript
		}
		view.Turns = append(view.Turns, NormalizedTurn{
			RecordIndex:   i,
			Role:          role,
			Kind:          kind,
			MessageID:     nestedMessageID(record),
			Text:          text,
			Model:         model,
			ResponseModel: responseModel,
			ModelSource:   modelSource,
			Provider:      firstStringDeep(record, "model_provider"),
			Reasoning:     reasoning,
			ID:            firstStringDeep(record, "id", "uuid"),
			ParentID:      firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"),
			TurnID:        firstStringDeep(record, "turn_id"),
			Timestamp:     firstStringDeep(record, "timestamp", "created_at"),
		})
	}
	resolveSlashCommands(view.Turns)
	view.ToolCalls = dedupeToolCalls(candidates)
	linkToolResults(view.ToolCalls, view.ToolResults)
	view.Tokens = tokens.usage()
	view.HookFinals = reconcileHookFinals(bundle, view.Turns)
	return view, nil
}

// compactionsObservable reports whether a bundle can show how many times its
// session was compacted: only a Claude Code bundle from filter 5 on retains the
// compact_boundary record and the isCompactSummary flag. For anything else the
// count is unknown, not zero.
func compactionsObservable(bundle SourceBundle) bool {
	if bundle.harness() != "claude" {
		return false
	}
	version, err := strconv.Atoi(strings.TrimSpace(bundle.Capture.FilterVersion))
	return err == nil && version >= 5
}

// harness is the bundle's harness name in its one canonical spelling
// ("claude-code" is "claude"). Every harness-specific rule compares this,
// never the raw recorded name.
func (b SourceBundle) harness() string { return CanonicalHarness(b.Capture.Harness.Name) }

// nestedMessageID returns the id of the record's nested message object
// (Claude's message.id), which streamed records of one response share.
func nestedMessageID(record map[string]any) string {
	if message, ok := record["message"].(map[string]any); ok {
		return firstString(message, "id")
	}
	return ""
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

func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, _ := record[key].(string); value != "" {
			return value
		}
	}
	return ""
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

// IsParseError supports the source-first publication flow.
func IsParseError(err error) bool {
	var target *ParseError
	return errors.As(err, &target)
}
