package archive

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// A session can report a final response per turn that the transcript does
// not hold: the oldest go, before any exchange, and the newest is kept.
func TestFitTranscriptDropsTheOldestHookFinalsBeforeAnyExchange(t *testing.T) {
	t.Parallel()
	in := bigTranscript(3)
	for i := range 4000 {
		in.HookFinals = append(in.HookFinals, fmt.Sprintf("final %d %s", i, strings.Repeat("w", 60)))
	}
	measure := transcriptSize(false)
	base := len(in.Exchanges)
	limit := measure(in) / 3
	out, fits := FitTranscript(in, limit, false, measure)
	if !fits || measure(out) > limit {
		t.Fatalf("fits=%v size=%d limit %d", fits, measure(out), limit)
	}
	if len(out.Exchanges) != base || len(out.HookFinals) == 0 || !strings.HasPrefix(out.HookFinals[len(out.HookFinals)-1], "final 3999 ") {
		t.Fatalf("%d exchanges, %d hook finals, want the newest kept and no exchange dropped", len(out.Exchanges), len(out.HookFinals))
	}
	last := out.Elisions[len(out.Elisions)-1]
	if last.Kind != TranscriptElisionOldestHookFinals || last.Count != 4000-len(out.HookFinals) {
		t.Fatalf("elisions = %+v, %d hook finals remain", out.Elisions, len(out.HookFinals))
	}
	// However small the limit, the newest is kept.
	out, fits = FitTranscript(in, 1, false, measure)
	if fits || len(out.HookFinals) != 1 || !strings.HasPrefix(out.HookFinals[0], "final 3999 ") {
		t.Fatalf("fits=%v, hook finals = %d", fits, len(out.HookFinals))
	}
}

// The person's own shell and slash commands are cut like their prompts.
func TestFitTranscriptTruncatesLongCommandsWithPrompts(t *testing.T) {
	t.Parallel()
	in := Transcript{}
	for i := range 6 {
		in.Exchanges = append(in.Exchanges, TranscriptExchange{Kind: TranscriptExchangePrompt, Text: "p", Steps: []TranscriptStep{
			{Kind: TranscriptStepShell, Text: strings.Repeat("é", 30000)},
			{Kind: TranscriptStepCommand, Text: fmt.Sprintf("/cmd %d", i)},
		}})
	}
	measure := transcriptSize(false)
	const limit = 20000
	out, fits := FitTranscript(in, limit, false, measure)
	if !fits || measure(out) > limit {
		t.Fatalf("fits=%v size=%d", fits, measure(out))
	}
	cut := 0
	for _, exchange := range out.Exchanges {
		if step := exchange.Steps[0]; step.TextTruncated {
			cut++
			if len(step.Text) > handoffPromptCap || !utf8.ValidString(step.Text) {
				t.Fatalf("command not cut to a valid %d bytes: %d", handoffPromptCap, len(step.Text))
			}
		}
	}
	if cut == 0 || out.Elisions[0].Kind != TranscriptElisionPromptText || out.Elisions[0].Count != cut {
		t.Fatalf("%d commands cut; elisions %+v", cut, out.Elisions)
	}
}

// When the newest exchange alone is too much, its newest reply is cut, not
// dropped with the older steps around it.
func TestFitTranscriptCutsTheNewestReplyBeforeDroppingIt(t *testing.T) {
	t.Parallel()
	exchange := TranscriptExchange{Kind: TranscriptExchangePrompt, Text: "go"}
	for i := range 30 {
		exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepText, Text: fmt.Sprintf("step %d %s", i, strings.Repeat("é", 20000))})
	}
	in := Transcript{Exchanges: []TranscriptExchange{exchange}}
	measure := transcriptSize(false)
	const limit = 70000
	out, fits := FitTranscript(in, limit, false, measure)
	if !fits || measure(out) > limit {
		t.Fatalf("fits=%v size=%d", fits, measure(out))
	}
	steps := out.Exchanges[0].Steps
	newest := steps[len(steps)-1]
	if len(steps) != 30 || !strings.HasPrefix(newest.Text, "step 29 ") || !newest.TextTruncated || len(newest.Text) > handoffPromptCap {
		t.Fatalf("%d steps, newest %d bytes, truncated=%v", len(steps), len(newest.Text), newest.TextTruncated)
	}
	if e := out.Elisions[len(out.Elisions)-1]; e.Kind != TranscriptElisionAssistantText || e.First != 1 || e.Last != 1 || e.Count == 0 {
		t.Fatalf("elisions = %+v", out.Elisions)
	}
}
