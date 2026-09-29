package archive

import (
	"reflect"
	"strings"
)

// Transcript is a filtered session arranged for a person to read: each
// human prompt and what the agent did before the next one, in the same
// exchanges BuildHandoff arranges, without a budget. Every string is display
// text (see displayText): lines are kept, and no terminal control sequence
// survives.
type Transcript struct {
	Exchanges []HandoffExchange
	// HookFinals are final responses a lifecycle hook reported that the
	// retained transcript does not carry: no turn matched the hook's
	// message or turn ID, and no assistant text in the session reads the
	// same. A subagent's final belongs to its own session and is left out.
	HookFinals []string
	// ToolResultsUnavailable is true when the harness records no tool
	// results at all (Cursor), so their absence can be stated once.
	ToolResultsUnavailable bool
}

// BuildTranscript arranges a filtered bundle for reading. Tool results are
// trimmed to opts' limits; prompts and assistant text are kept whole.
func BuildTranscript(bundle SourceBundle, opts HandoffOptions) (Transcript, error) {
	view, err := ParseNormalized(bundle)
	if err != nil {
		return Transcript{}, err
	}
	var exchanges []HandoffExchange
	toolResultsUnavailable := bundle.harness() == "cursor" && len(view.ToolResults) == 0 && len(view.ToolCalls) > 0
	if len(bundle.NativeRecords) == 0 && len(bundle.NativeText) > 0 {
		exchanges, _ = textTranscriptExchanges(bundle.NativeText, opts)
		toolResultsUnavailable = false
	} else {
		exchanges, _, _, _ = selectHandoffExchanges(handoffEvents(view), workspaceRoot(bundle), opts)
	}
	t := Transcript{
		Exchanges:              exchanges,
		HookFinals:             unmatchedHookFinals(bundle, view, exchanges),
		ToolResultsUnavailable: toolResultsUnavailable,
	}
	out, _ := displayValue(reflect.ValueOf(t)).Interface().(Transcript)
	return out, nil
}

// unmatchedHookFinals returns the text of each hook-reported final response
// that reconciliation could not tie to a transcript turn. Claude Code's Stop
// hook names no message ID, so its final is never reconciled by identity;
// comparing text here only keeps the reader from seeing the last reply
// twice, and never changes what the archive records.
func unmatchedHookFinals(bundle SourceBundle, view NormalizedView, exchanges []HandoffExchange) []string {
	shown := map[string]bool{}
	for _, exchange := range exchanges {
		for _, step := range exchange.Steps {
			if step.Kind == HandoffStepText {
				shown[strings.TrimSpace(step.Text)] = true
			}
		}
	}
	var out []string
	for _, final := range view.HookFinals {
		if final.Status != HookFinalStatusUnreconciledIdentity || final.EvidenceIndex >= len(bundle.SupplementalEvidence) {
			continue
		}
		text := strings.TrimSpace(firstString(bundle.SupplementalEvidence[final.EvidenceIndex].Payload, "text"))
		if text == "" || shown[text] {
			continue
		}
		shown[text] = true
		out = append(out, text)
	}
	return out
}
