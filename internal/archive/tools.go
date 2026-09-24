package archive

import (
	"encoding/json"
	"sort"
	"strings"
)

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
		calls[index].resultText = result.text
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
					Model: model, Reasoning: reasoning, Name: tool, Input: arguments, raw: item,
				}})
			}
			if kind == "item_completed" {
				if completed, ok := item["item"].(map[string]any); ok && completedItemTypes[strings.ToLower(strings.TrimSpace(firstString(completed, "type")))] && !isCodexStartupShell(completed) {
					calls = append(calls, toolCallCandidate{completionEcho: true, call: NormalizedToolCall{
						RecordIndex: index, CallID: firstString(completed, "call_id", "id"), ParentID: parent,
						Model: model, Reasoning: reasoning, Name: firstString(completed, "name", "tool_name"),
						Input: toolArguments(completed), raw: completed,
					}})
				}
			}
			if toolResultTypes[kind] {
				isError, _ := item["is_error"].(bool)
				output := toolResultOutput(item)
				results = append(results, NormalizedToolResult{
					RecordIndex: index, CallID: firstString(item, "call_id", "tool_use_id"),
					IsError: isError, OutputBytes: len(output), text: output,
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
			for _, key := range walkKeys(item, toolResultTypes[kind]) {
				walk(item[key])
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

// walkKeys are the keys of item toolActivity descends into, in sorted order
// so the calls of one record always come out in the same order. It never
// descends into a call's arguments (toolArgumentKeys: a tool's input may
// itself contain {"type":"tool_use"} shapes, which are data, not calls) or,
// for a result, into its output.
func walkKeys(item map[string]any, isResult bool) []string {
	keys := make([]string, 0, len(item))
	for key := range item {
		if toolArgumentKeys[key] || (isResult && (key == "content" || key == "output")) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
// length is what OutputBytes reports. Current Codex writes a function or custom
// tool's output as a list of {type: input_text, text} blocks rather than one
// string; their text is joined the same way message content is.
func toolResultOutput(item map[string]any) string {
	switch output := item["output"].(type) {
	case string:
		return output
	case []any, map[string]any:
		return contentText(output)
	}
	if content, present := item["content"]; present {
		return contentText(content)
	}
	if result, ok := item["result"].(string); ok {
		return result
	}
	return ""
}
