package archive

// dedupeToolCalls drops the completion echo a harness writes for a call it
// already reported. Codex emits both a response_item for the call and a later
// item_completed for the same work; counting both would double every Codex
// tool call. An echo with no identity of its own is kept, since nothing proves
// it duplicates another record.
func dedupeToolCalls(candidates []toolCallCandidate) []NormalizedToolCall {
	reported := map[string]bool{}
	for _, candidate := range candidates {
		if !candidate.CompletionEcho && candidate.Call.CallID != "" {
			reported[candidate.Call.CallID] = true
		}
	}
	var out []NormalizedToolCall
	for _, candidate := range candidates {
		if candidate.CompletionEcho && candidate.Call.CallID != "" && reported[candidate.Call.CallID] {
			continue
		}
		out = append(out, candidate.Call)
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
		calls[index].ResultText = result.Text
		if !result.RecordedAt.IsZero() {
			at := result.RecordedAt
			calls[index].ResultAt = &at
		}
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

// toolCallCandidate carries whether a call came from a completion event, which
// dedupeToolCalls needs and the published view does not.
type toolCallCandidate = ToolCallCandidate
