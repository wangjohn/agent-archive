package archive

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// FitTranscript returns a copy of t trimmed until measure(copy) is at most
// maxBytes, applying the same budget steps as FitHandoff in the same order
// and recording each in Elisions: tool and command output dropped, runs of
// tool calls collapsed to counts by name, agent messages shortened, then
// long prompts truncated. Each step goes to the oldest exchanges first and
// to no more of them than needed; the protected tail of recent steps is
// never touched by the first three. Prompts (and the person's shell and
// slash commands) are truncated before any exchange is dropped, and when
// even that is not enough long hook-reported final responses are shortened,
// the oldest of them dropped (keeping the newest), and the oldest exchanges
// dropped, always keeping the newest. When the newest alone is still too
// much its long agent messages are shortened and then its oldest steps go,
// so that the result is bounded (unlike a handoff, which keeps every
// prompt).
// showOutput says whether the rendering prints tool results and command
// output at all (`show --transcript --full`); when it does not, dropping
// them would change nothing, so that step is skipped. fits is false when
// the result is still over budget after every step. maxBytes <= 0 means no
// budget.
func FitTranscript(t Transcript, maxBytes int, showOutput bool, measure func(Transcript) int) (Transcript, bool) {
	out := cloneTranscript(t)
	if maxBytes <= 0 || measure(out) <= maxBytes {
		return out, true
	}
	type step struct {
		kind  TranscriptElisionKind
		apply func(steps []TranscriptStep, limit int) ([]TranscriptStep, int)
	}
	steps := []step{
		{TranscriptElisionToolOutput, dropTranscriptOutput},
		{TranscriptElisionToolCalls, collapseTranscriptToolCalls},
		{TranscriptElisionAssistantText, shortenTranscriptText},
	}
	if !showOutput {
		steps = steps[1:]
	}
	for _, s := range steps {
		// Protection is computed once, before the step, as FitHandoff does.
		limits := protectedTranscriptStart(out.Exchanges)
		var fits bool
		out, fits = SmallestFit(len(out.Exchanges), maxBytes, measure, func(k int) Transcript {
			trial := cloneTranscript(out)
			elision := TranscriptElision{Kind: s.kind}
			for i := range k {
				var n int
				trial.Exchanges[i].Steps, n = s.apply(trial.Exchanges[i].Steps, limits[i])
				elision = elision.with(i, n)
			}
			return trial.withElision(elision)
		})
		if fits {
			return out, true
		}
	}
	base := out
	out, fits := SmallestFit(len(base.Exchanges), maxBytes, measure, func(k int) Transcript {
		trial := cloneTranscript(base)
		elision := TranscriptElision{Kind: TranscriptElisionPromptText}
		for i := range k {
			exchange := &trial.Exchanges[i]
			if exchange.Kind == TranscriptExchangePrompt && len(exchange.Text) > handoffPromptCap {
				exchange.Text, exchange.TextTruncated = TruncateUTF8(exchange.Text, handoffPromptCap), true
				elision = elision.with(i, 1)
			}
			// What the person typed as a shell or slash command is as much
			// theirs as a prompt.
			for j := range exchange.Steps {
				step := &exchange.Steps[j]
				if (step.Kind == TranscriptStepShell || step.Kind == TranscriptStepCommand) && len(step.Text) > handoffPromptCap {
					step.Text, step.TextTruncated = TruncateUTF8(step.Text, handoffPromptCap), true
					elision = elision.with(i, 1)
				}
			}
		}
		return trial.withElision(elision)
	})
	if fits {
		return out, true
	}
	base = out
	if hasLongHookFinal(base.HookFinals) {
		out, fits = SmallestFit(len(base.HookFinals), maxBytes, measure, func(k int) Transcript {
			trial := cloneTranscript(base)
			n := 0
			for i := range k {
				if len(trial.HookFinals[i]) > handoffPromptCap {
					trial.HookFinals[i] = TruncateUTF8(trial.HookFinals[i], handoffPromptCap) + hookFinalShortened
					n++
				}
			}
			return trial.withElision(TranscriptElision{Kind: TranscriptElisionHookFinals, Count: n})
		})
		if fits {
			return out, true
		}
		base = out
	}
	// The newest hook-reported final response is kept; the older ones go
	// before any exchange does, since a long session can report thousands.
	out, fits = SmallestFit(max(len(base.HookFinals)-1, 0), maxBytes, measure, func(k int) Transcript {
		trial := cloneTranscript(base)
		trial.HookFinals = trial.HookFinals[k:]
		return trial.withElision(TranscriptElision{Kind: TranscriptElisionOldestHookFinals, Count: k})
	})
	if fits {
		return out, true
	}
	base = out
	out, fits = SmallestFit(max(len(base.Exchanges)-1, 0), maxBytes, measure, func(k int) Transcript {
		trial := cloneTranscript(base)
		trial.Exchanges = trial.Exchanges[k:]
		return trial.withElision(TranscriptElision{Kind: TranscriptElisionOldestExchanges, First: 1, Last: k, Count: k})
	})
	if fits || len(out.Exchanges) == 0 {
		return out, fits
	}
	// Only the newest exchange is left, and it alone is over budget: a long
	// autonomous run is one prompt and thousands of steps. Shorten its long
	// agent messages, oldest first, so that its newest reply is cut rather
	// than dropped whole.
	base = out
	last := len(base.Exchanges) - 1
	out, fits = SmallestFit(len(base.Exchanges[last].Steps), maxBytes, measure, func(k int) Transcript {
		trial := cloneTranscript(base)
		n := 0
		for i := range k {
			step := &trial.Exchanges[last].Steps[i]
			if (step.Kind == TranscriptStepText || step.Kind == TranscriptStepSummary) && len(step.Text) > handoffPromptCap {
				step.Text, step.TextTruncated = TruncateUTF8(step.Text, handoffPromptCap), true
				n++
			}
		}
		return trial.withElision(TranscriptElision{Kind: TranscriptElisionAssistantText, First: last + 1, Last: last + 1, Count: n})
	})
	if fits {
		return out, true
	}
	// Then drop its oldest steps, so that the bound holds however the
	// session is shaped; the newest steps are the ones that say where it
	// stands.
	base = out
	return SmallestFit(len(base.Exchanges[last].Steps), maxBytes, measure, func(k int) Transcript {
		trial := cloneTranscript(base)
		trial.Exchanges[last].Steps = trial.Exchanges[last].Steps[k:]
		return trial.withElision(TranscriptElision{Kind: TranscriptElisionOldestSteps, Count: k})
	})
}

// hookFinalShortened marks a hook-reported final response FitTranscript cut.
const hookFinalShortened = " …(shortened)"

func hasLongHookFinal(finals []string) bool {
	return slices.ContainsFunc(finals, func(s string) bool { return len(s) > handoffPromptCap })
}

// TranscriptElision records one FitTranscript step: what it removed and from
// which exchanges (1-based, inclusive).
type TranscriptElision struct {
	Kind  TranscriptElisionKind
	First int
	Last  int
	Count int
}

// TranscriptElisionKind names one FitTranscript step.
type TranscriptElisionKind string

// TranscriptElision kinds, in the order FitTranscript applies them. The first
// three never touch the protected tail of recent steps.
const (
	// TranscriptElisionToolOutput drops tool results and shell command
	// output.
	TranscriptElisionToolOutput TranscriptElisionKind = "tool_output"
	// TranscriptElisionToolCalls collapses runs of tool calls to per-tool
	// counts.
	TranscriptElisionToolCalls TranscriptElisionKind = "tool_calls"
	// TranscriptElisionAssistantText shortens agent messages.
	TranscriptElisionAssistantText TranscriptElisionKind = "assistant_text"
	// TranscriptElisionPromptText truncates long prompts.
	TranscriptElisionPromptText TranscriptElisionKind = "prompt_text"
	// TranscriptElisionHookFinals shortens long final responses a hook
	// reported that the transcript does not hold.
	TranscriptElisionHookFinals TranscriptElisionKind = "hook_finals"
	// TranscriptElisionOldestHookFinals drops the oldest final responses a
	// hook reported, keeping the newest.
	TranscriptElisionOldestHookFinals TranscriptElisionKind = "oldest_hook_finals"
	// TranscriptElisionOldestExchanges drops the oldest exchanges
	// altogether.
	TranscriptElisionOldestExchanges TranscriptElisionKind = "oldest_exchanges"
	// TranscriptElisionOldestSteps drops the oldest steps of the newest
	// exchange, when it alone is over budget.
	TranscriptElisionOldestSteps TranscriptElisionKind = "oldest_steps"
)

// SmallestFit returns apply(k) for the smallest k in 1..n whose result
// measures at most maxBytes, and true; when none does, it returns apply(n)
// and false. apply must not modify what it was built from, and its result
// must not grow with k, which is what makes a binary search valid: a
// handful of measurements rather than one per k, which matters when measure
// renders a multi-megabyte session. With n == 0 it returns apply(0).
func SmallestFit[T any](n, maxBytes int, measure func(T) int, apply func(k int) T) (T, bool) {
	all := apply(n)
	if measure(all) > maxBytes {
		return all, false
	}
	best := all
	lo, hi := 1, n // the answer is in [lo, hi]; apply(hi) fits
	for lo < hi {
		mid := (lo + hi) / 2
		if trial := apply(mid); measure(trial) <= maxBytes {
			hi, best = mid, trial
		} else {
			lo = mid + 1
		}
	}
	return best, true
}

// with counts n trimmed things at exchange index i (0-based) in e, whose
// First and Last are 1-based.
func (e TranscriptElision) with(i, n int) TranscriptElision {
	if n == 0 {
		return e
	}
	if e.First == 0 {
		e.First = i + 1
	}
	e.Last, e.Count = i+1, e.Count+n
	return e
}

// withElision is t with elision recorded, when it trimmed anything.
func (t Transcript) withElision(elision TranscriptElision) Transcript {
	if elision.Count > 0 {
		t.Elisions = append(t.Elisions, elision)
	}
	return t
}

// DescribeTranscriptElisions reads a transcript's Elisions as one sentence fragment,
// such as "output of 12 tool calls or commands in exchanges 1–4; 3 agent
// messages in exchange 2 shortened".
func DescribeTranscriptElisions(elisions []TranscriptElision) string {
	parts := make([]string, 0, len(elisions))
	for _, e := range elisions {
		span := fmt.Sprintf("exchange %d", e.First)
		if e.Last != e.First {
			span = fmt.Sprintf("exchanges %d–%d", e.First, e.Last)
		}
		switch e.Kind {
		case TranscriptElisionToolOutput:
			parts = append(parts, fmt.Sprintf("output of %s in %s", countOf(e.Count, "tool call or command", "tool calls or commands"), span))
		case TranscriptElisionToolCalls:
			parts = append(parts, fmt.Sprintf("%s in %s collapsed to counts", countOf(e.Count, "tool call", "tool calls"), span))
		case TranscriptElisionAssistantText:
			parts = append(parts, fmt.Sprintf("%s in %s shortened", countOf(e.Count, "agent message", "agent messages"), span))
		case TranscriptElisionPromptText:
			parts = append(parts, fmt.Sprintf("%s in %s truncated", countOf(e.Count, "long prompt or command", "long prompts or commands"), span))
		case TranscriptElisionHookFinals:
			parts = append(parts, countOf(e.Count, "final response", "final responses")+" shortened")
		case TranscriptElisionOldestHookFinals:
			parts = append(parts, fmt.Sprintf("the oldest %s dropped", countOf(e.Count, "final response", "final responses")))
		case TranscriptElisionOldestExchanges:
			parts = append(parts, fmt.Sprintf("the oldest %s dropped", countOf(e.Count, "exchange", "exchanges")))
		case TranscriptElisionOldestSteps:
			parts = append(parts, fmt.Sprintf("the oldest %s of the newest exchange dropped", countOf(e.Count, "step", "steps")))
		}
	}
	return strings.Join(parts, "; ")
}

// protectedTranscriptStart is protectedStart for a transcript: for each
// exchange, the index of its first protected step.
func protectedTranscriptStart(exchanges []TranscriptExchange) []int {
	counts := make([]HandoffExchange, len(exchanges))
	for i, exchange := range exchanges {
		counts[i].Steps = make([]HandoffStep, len(exchange.Steps))
	}
	return protectedStart(counts, handoffKeptExchanges, handoffKeptSteps)
}

// dropTranscriptOutput drops the tool results and command output in steps
// before limit. A local slash command's reply is the app's own short
// answer, always shown, and is kept.
func dropTranscriptOutput(steps []TranscriptStep, limit int) ([]TranscriptStep, int) {
	n := 0
	for i := range steps[:limit] {
		step := &steps[i]
		if step.Tool != nil && step.Tool.Result != "" {
			step.Tool.Result, step.Tool.ResultOmitted = "", true
			n++
		}
		if (step.Kind == TranscriptStepShell || step.Kind == TranscriptStepOutput) && step.Output != "" {
			step.Output = ""
			n++
		}
	}
	return steps, n
}

// collapseTranscriptToolCalls replaces the tool-call steps before limit
// with one collapsed step, at the position of the first, that counts them
// by name.
func collapseTranscriptToolCalls(steps []TranscriptStep, limit int) ([]TranscriptStep, int) {
	counts := map[string]int{}
	var order []string
	out := make([]TranscriptStep, 0, len(steps))
	at, total := -1, 0
	for i, step := range steps {
		if i >= limit || step.Tool == nil {
			out = append(out, step)
			continue
		}
		if at < 0 {
			at = len(out)
			out = append(out, TranscriptStep{Kind: TranscriptStepCollapsed})
		}
		if counts[step.Tool.Name] == 0 {
			order = append(order, step.Tool.Name)
		}
		counts[step.Tool.Name]++
		total++
	}
	if total == 0 {
		return steps, 0
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%s ×%d", name, counts[name]))
	}
	noun := "tool calls"
	if total == 1 {
		noun = "tool call"
	}
	out[at].Text = fmt.Sprintf("%d %s: %s", total, noun, strings.Join(parts, ", "))
	return out, total
}

// shortenTranscriptText cuts the agent's replies and compaction summaries
// before limit to handoffAssistantTextCap bytes.
func shortenTranscriptText(steps []TranscriptStep, limit int) ([]TranscriptStep, int) {
	n := 0
	for i := range steps[:limit] {
		step := &steps[i]
		if (step.Kind == TranscriptStepText || step.Kind == TranscriptStepSummary) && len(step.Text) > handoffAssistantTextCap {
			step.Text, step.TextTruncated = TruncateUTF8(step.Text, handoffAssistantTextCap), true
			n++
		}
	}
	return steps, n
}

// cloneTranscript copies everything FitTranscript mutates.
func cloneTranscript(t Transcript) Transcript {
	out := t
	out.Elisions = append([]TranscriptElision(nil), t.Elisions...)
	out.HookFinals = append([]string(nil), t.HookFinals...)
	out.Exchanges = make([]TranscriptExchange, len(t.Exchanges))
	for i, exchange := range t.Exchanges {
		copied := exchange
		copied.Steps = make([]TranscriptStep, len(exchange.Steps))
		for j, step := range exchange.Steps {
			if step.Tool != nil {
				tool := *step.Tool
				step.Tool = &tool
			}
			copied.Steps[j] = step
		}
		out.Exchanges[i] = copied
	}
	return out
}

// countOf is n of a noun, such as "1 step" or "3 steps".
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
