package archive

import (
	"reflect"
	"regexp"
	"strings"
)

// Transcript is a filtered session arranged for a person to read: each
// human prompt and what happened before the next one, with no budget. Unlike
// a handoff, which keeps only what another agent needs, it also shows what
// the person saw in the app: `!` shell commands and their output, local
// slash commands (/model, /compact) and theirs, and notices the app itself
// posted, such as a background task finishing. Every string is display text
// (see displayText): lines are kept, and no terminal control sequence
// survives.
type Transcript struct {
	Exchanges []TranscriptExchange
	// HookFinals are final responses a lifecycle hook reported that the
	// retained transcript does not carry: no turn matched the hook's
	// message or turn ID, and no assistant text in the session reads the
	// same. A subagent's final belongs to its own session and is left out.
	HookFinals []string
	// ToolResultsUnavailable is true when the harness records no tool
	// results at all (Cursor), so their absence can be stated once.
	ToolResultsUnavailable bool
}

// TranscriptExchangeKind says what starts a TranscriptExchange.
type TranscriptExchangeKind string

// TranscriptExchange kinds.
const (
	// TranscriptExchangeLeading holds activity before the first prompt.
	TranscriptExchangeLeading TranscriptExchangeKind = "leading"
	// TranscriptExchangePrompt starts at a human prompt.
	TranscriptExchangePrompt TranscriptExchangeKind = "prompt"
	// TranscriptExchangeNotification starts at a notice the app posted on
	// its own; the agent's reply to it follows.
	TranscriptExchangeNotification TranscriptExchangeKind = "notification"
)

// TranscriptExchange is a prompt (or app notice) and everything after it
// until the next one. Text is the prompt, or a one-line description of the
// notice.
type TranscriptExchange struct {
	Kind      TranscriptExchangeKind
	Text      string
	Timestamp string
	Steps     []TranscriptStep
}

// TranscriptStepKind says which of the TranscriptStep shapes a step is.
type TranscriptStepKind string

// TranscriptStep kinds.
const (
	// TranscriptStepText is assistant text.
	TranscriptStepText TranscriptStepKind = "text"
	// TranscriptStepTool is a tool call.
	TranscriptStepTool TranscriptStepKind = "tool"
	// TranscriptStepShell is a `!` shell command the person ran, with its
	// output.
	TranscriptStepShell TranscriptStepKind = "shell"
	// TranscriptStepCommand is a local slash command no assistant answered
	// (/model, /compact), with its output.
	TranscriptStepCommand TranscriptStepKind = "command"
	// TranscriptStepSummary is the summary Claude Code wrote when it
	// compacted the session.
	TranscriptStepSummary TranscriptStepKind = "summary"
	// TranscriptStepOutput is command output with no command before it.
	TranscriptStepOutput TranscriptStepKind = "output"
)

// TranscriptStep is one thing that happened in an exchange. Text is the
// reply, command line, or summary; Output is a command's trimmed output;
// Tool is set for a tool call.
type TranscriptStep struct {
	Kind   TranscriptStepKind
	Text   string
	Output string
	Tool   *HandoffToolCall
}

// BuildTranscript arranges a filtered bundle for reading. Tool results and
// command output are trimmed to opts' limits; prompts and assistant text are
// kept whole.
func BuildTranscript(bundle SourceBundle, opts HandoffOptions) (Transcript, error) {
	view, err := ParseNormalized(bundle)
	if err != nil {
		return Transcript{}, err
	}
	var exchanges []TranscriptExchange
	toolResultsUnavailable := bundle.harness() == "cursor" && len(view.ToolResults) == 0 && len(view.ToolCalls) > 0
	if len(bundle.NativeRecords) == 0 && len(bundle.NativeText) > 0 {
		// A Cursor text transcript: role sections, read as handoff reads
		// them.
		handoff, _ := textTranscriptExchanges(bundle.NativeText, opts)
		exchanges = fromHandoffExchanges(handoff)
		toolResultsUnavailable = false
	} else {
		exchanges = transcriptExchanges(handoffEvents(view), workspaceRoot(bundle), opts)
	}
	t := Transcript{
		Exchanges:              exchanges,
		HookFinals:             unmatchedHookFinals(bundle, view, exchanges),
		ToolResultsUnavailable: toolResultsUnavailable,
	}
	out, _ := displayValue(reflect.ValueOf(t)).Interface().(Transcript)
	return out, nil
}

// transcriptExchanges groups turns and tool calls in record order.
func transcriptExchanges(events []handoffEvent, root string, opts HandoffOptions) []TranscriptExchange {
	exchanges := []TranscriptExchange{}
	current := TranscriptExchange{Kind: TranscriptExchangeLeading}
	flush := func() {
		if current.Kind != TranscriptExchangeLeading || len(current.Steps) > 0 {
			exchanges = append(exchanges, current)
		}
	}
	for _, event := range events {
		if event.call != nil {
			if tool := handoffToolCall(event.call, root, opts); tool != nil {
				current.Steps = append(current.Steps, TranscriptStep{Kind: TranscriptStepTool, Tool: tool})
			}
			continue
		}
		turn := event.turn
		switch turn.Kind {
		case TurnKindHumanPrompt:
			flush()
			current = TranscriptExchange{Kind: TranscriptExchangePrompt, Text: cleanPrompt(turn.Text), Timestamp: turn.Timestamp}
		case TurnKindHarnessNotification:
			// A new exchange, so the reply that follows is not credited to
			// the prompt before it.
			flush()
			current = TranscriptExchange{Kind: TranscriptExchangeNotification, Text: notificationText(turn.Text), Timestamp: turn.Timestamp}
		case TurnKindAssistant:
			if text := strings.TrimSpace(turn.Text); text != "" {
				current.Steps = append(current.Steps, TranscriptStep{Kind: TranscriptStepText, Text: text})
			}
		case TurnKindShellCommand:
			if command := strings.TrimSpace(stripHarnessTag(turn.Text, "bash-input")); command != "" {
				current.Steps = append(current.Steps, TranscriptStep{Kind: TranscriptStepShell, Text: command})
			}
		case TurnKindLocalCommand:
			if command := cleanPrompt(turn.Text); command != "" {
				current.Steps = append(current.Steps, TranscriptStep{Kind: TranscriptStepCommand, Text: command})
			}
		case TurnKindCommandOutput:
			output := trimResult(commandOutput(turn.Text), opts.resultLines(), opts.resultBytes())
			if output == "" {
				continue
			}
			if step := lastCommandStep(current.Steps); step != nil {
				step.Output = output
			} else {
				current.Steps = append(current.Steps, TranscriptStep{Kind: TranscriptStepOutput, Output: output})
			}
		case TurnKindCompactSummary:
			if text := strings.TrimSpace(turn.Text); text != "" {
				current.Steps = append(current.Steps, TranscriptStep{Kind: TranscriptStepSummary, Text: text})
			}
		case TurnKindToolResult, TurnKindHarnessMeta:
			// Tool results are shown with their calls; harness metadata is
			// not something the person saw.
		}
	}
	flush()
	return exchanges
}

// lastCommandStep is the latest shell or local command in steps that has
// no output yet. Only tool calls and a compaction summary may come between
// a command and its output.
func lastCommandStep(steps []TranscriptStep) *TranscriptStep {
	for i := len(steps) - 1; i >= 0; i-- {
		switch steps[i].Kind {
		case TranscriptStepShell, TranscriptStepCommand:
			if steps[i].Output != "" {
				return nil
			}
			return &steps[i]
		case TranscriptStepTool, TranscriptStepSummary:
		case TranscriptStepText, TranscriptStepOutput:
			return nil
		}
	}
	return nil
}

var (
	// commandOutputTag matches the tags Claude Code wraps command output in.
	commandOutputTag = regexp.MustCompile(`(?s)<(bash-stdout|bash-stderr|local-command-stdout|local-command-stderr)>(.*?)</(?:bash-stdout|bash-stderr|local-command-stdout|local-command-stderr)>`)
	// taskStatus reads a background task notice's status.
	taskStatus = regexp.MustCompile(`(?s)<status>\s*(.*?)\s*</status>`)
)

// commandOutput is the text of a command-output record: stdout, then
// stderr, without their tags. The caveat Claude Code adds before local
// command output is not output.
func commandOutput(text string) string {
	matches := commandOutputTag.FindAllStringSubmatch(text, -1)
	if matches == nil {
		if strings.Contains(text, "<local-command-caveat>") {
			return ""
		}
		return strings.TrimSpace(text)
	}
	var parts []string
	for _, m := range matches {
		if body := strings.Trim(m[2], "\n"); strings.TrimSpace(body) != "" {
			parts = append(parts, body)
		}
	}
	return strings.Join(parts, "\n")
}

// notificationText describes a notice the app posted: "Background task
// completed" for Claude Code's task notification.
func notificationText(text string) string {
	if strings.Contains(text, "<task-notification>") {
		if m := taskStatus.FindStringSubmatch(text); m != nil && m[1] != "" {
			return "Background task " + firstLine(m[1], handoffSummaryCap)
		}
		return "Background task notification"
	}
	return "App notification"
}

// fromHandoffExchanges carries a text transcript's handoff exchanges over.
func fromHandoffExchanges(handoff []HandoffExchange) []TranscriptExchange {
	out := make([]TranscriptExchange, 0, len(handoff))
	for _, h := range handoff {
		kind := TranscriptExchangePrompt
		if h.Prompt == "" {
			kind = TranscriptExchangeLeading
		}
		exchange := TranscriptExchange{Kind: kind, Text: h.Prompt, Timestamp: h.Timestamp}
		for _, step := range h.Steps {
			switch step.Kind {
			case HandoffStepText:
				exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepText, Text: step.Text})
			case HandoffStepTool:
				exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepTool, Tool: step.Tool})
			case HandoffStepShell:
				exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepShell, Text: step.Text})
			case HandoffStepSummary:
				exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepSummary, Text: step.Text})
			case HandoffStepCollapsed:
				// Only a budget collapses steps, and a transcript has none.
			}
		}
		out = append(out, exchange)
	}
	return out
}

// unmatchedHookFinals returns the text of each hook-reported final response
// that reconciliation could not tie to a transcript turn. Claude Code's Stop
// hook names no message ID, so its final is never reconciled by identity;
// comparing text here only keeps the reader from seeing the last reply
// twice, and never changes what the archive records.
func unmatchedHookFinals(bundle SourceBundle, view NormalizedView, exchanges []TranscriptExchange) []string {
	shown := map[string]bool{}
	for _, exchange := range exchanges {
		for _, step := range exchange.Steps {
			if step.Kind == TranscriptStepText {
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
