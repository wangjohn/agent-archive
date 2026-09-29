package archive

import (
	"fmt"
	"strings"
	"testing"
)

// bigTranscript is n exchanges, each a prompt, a long reply, three tool
// calls with results, and a shell command with output, so every trimming
// step has something to do.
func bigTranscript(n int) Transcript {
	var t Transcript
	for i := range n {
		exchange := TranscriptExchange{Kind: TranscriptExchangePrompt, Text: fmt.Sprintf("prompt %d %s", i, strings.Repeat("p", 3000))}
		exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepText, Text: fmt.Sprintf("reply %d %s", i, strings.Repeat("r", 1000))})
		for j := range 3 {
			tool := &HandoffToolCall{Name: []string{"Bash", "Read", "Bash"}[j], Summary: "s", Result: strings.Repeat("o", 500), ResultLines: 1, ResultBytes: 500}
			exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepTool, Tool: tool})
		}
		exchange.Steps = append(exchange.Steps, TranscriptStep{Kind: TranscriptStepShell, Text: "make", Output: strings.Repeat("s", 500)})
		t.Exchanges = append(t.Exchanges, exchange)
	}
	return t
}

// transcriptSize counts the bytes a rendering would carry: every prompt,
// reply, and tool line, and, when showOutput, every result and command
// output.
func transcriptSize(showOutput bool) func(Transcript) int {
	return func(t Transcript) int {
		size := len(DescribeTranscriptElisions(t.Elisions))
		for _, e := range t.Exchanges {
			size += len(e.Text)
			for _, s := range e.Steps {
				size += len(s.Text) + 20
				if s.Tool != nil {
					size += len(s.Tool.Name)
					if showOutput {
						size += len(s.Tool.Result)
					}
				}
				if showOutput {
					size += len(s.Output)
				}
			}
		}
		return size
	}
}

func elisionKinds(t Transcript) []TranscriptElisionKind {
	var kinds []TranscriptElisionKind
	for _, e := range t.Elisions {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

func TestFitTranscriptUnderLimitOrUnlimitedChangesNothing(t *testing.T) {
	t.Parallel()
	in := bigTranscript(4)
	measure := transcriptSize(true)
	for _, limit := range []int{0, -1, measure(in), measure(in) + 1000} {
		out, fits := FitTranscript(in, limit, true, measure)
		if !fits || len(out.Elisions) != 0 || measure(out) != measure(in) {
			t.Fatalf("limit %d: fits=%v elisions=%v", limit, fits, out.Elisions)
		}
	}
}

// Output goes first, then tool calls, then agent text, then prompts, and each
// step is applied only as far as needed, oldest exchanges first.
func TestFitTranscriptAppliesStepsInOrderOldestFirst(t *testing.T) {
	t.Parallel()
	in := bigTranscript(12)
	measure := transcriptSize(true)
	full := measure(in)
	var previous []TranscriptElisionKind
	for _, limit := range []int{full - 1000, full * 8 / 10, full * 6 / 10, 37200} {
		out, fits := FitTranscript(in, limit, true, measure)
		if !fits || measure(out) > limit {
			t.Fatalf("limit %d: fits=%v size=%d elisions=%v", limit, fits, measure(out), out.Elisions)
		}
		kinds := elisionKinds(out)
		if len(kinds) < len(previous) {
			t.Fatalf("limit %d applied fewer steps (%v) than a looser one (%v)", limit, kinds, previous)
		}
		for i, e := range out.Elisions {
			if e.First != 1 || e.Last > len(in.Exchanges) || e.Count == 0 {
				t.Fatalf("limit %d: elision %d = %+v, want a prefix starting at exchange 1", limit, i, e)
			}
		}
		previous = kinds
	}
	want := []TranscriptElisionKind{TranscriptElisionToolOutput, TranscriptElisionToolCalls, TranscriptElisionAssistantText, TranscriptElisionPromptText}
	if fmt.Sprint(previous) != fmt.Sprint(want) {
		t.Fatalf("the tightest limit applied %v, want %v", previous, want)
	}
	// Only as far as needed: a limit that dropping output alone meets
	// leaves every tool call.
	out, _ := FitTranscript(in, full-1000, true, measure)
	if fmt.Sprint(elisionKinds(out)) != fmt.Sprint([]TranscriptElisionKind{TranscriptElisionToolOutput}) {
		t.Fatalf("a slightly-over limit applied %v", elisionKinds(out))
	}
	for _, e := range out.Exchanges {
		for _, s := range e.Steps {
			if s.Kind == TranscriptStepCollapsed {
				t.Fatalf("collapsed tool calls when dropping output was enough")
			}
		}
	}
}

func TestFitTranscriptProtectsTheRecentTail(t *testing.T) {
	t.Parallel()
	in := bigTranscript(12)
	measure := transcriptSize(true)
	out, _ := FitTranscript(in, measure(in)*4/10, true, measure)
	last := out.Exchanges[len(out.Exchanges)-1]
	want := in.Exchanges[len(in.Exchanges)-1]
	for i, s := range last.Steps {
		if s.Kind == TranscriptStepCollapsed || s.TextTruncated || (s.Tool != nil && s.Tool.ResultOmitted) || s.Text != want.Steps[i].Text {
			t.Fatalf("protected step %d was trimmed: %+v", i, s)
		}
	}
}

// Without --full no result or output is shown, so dropping it would trim
// nothing and only lie in the footer.
func TestFitTranscriptSkipsOutputWhenItIsNotShown(t *testing.T) {
	t.Parallel()
	in := bigTranscript(12)
	measure := transcriptSize(false)
	out, fits := FitTranscript(in, measure(in)*8/10, false, measure)
	if !fits {
		t.Fatalf("did not fit")
	}
	for _, kind := range elisionKinds(out) {
		if kind == TranscriptElisionToolOutput {
			t.Fatalf("dropped output nobody sees: %v", out.Elisions)
		}
	}
	if len(out.Elisions) == 0 {
		t.Fatalf("nothing was trimmed")
	}
}

// Prompts are truncated before any exchange is dropped.
func TestFitTranscriptTruncatesPromptsBeforeDroppingExchanges(t *testing.T) {
	t.Parallel()
	in := bigTranscript(6)
	measure := transcriptSize(false)
	// Room for every exchange once prompts are cut to 2000 bytes each and the
	// three protected ones keep their replies.
	limit := len(in.Exchanges)*(handoffPromptCap+700) + 4500
	out, fits := FitTranscript(in, limit, false, measure)
	if !fits || len(out.Exchanges) != len(in.Exchanges) {
		t.Fatalf("fits=%v, %d of %d exchanges remain", fits, len(out.Exchanges), len(in.Exchanges))
	}
	// The oldest prompts are cut, no more of them than needed.
	if !out.Exchanges[0].TextTruncated || len(out.Exchanges[0].Text) != handoffPromptCap || out.Exchanges[len(out.Exchanges)-1].TextTruncated {
		t.Fatalf("prompts: first truncated=%v, last truncated=%v", out.Exchanges[0].TextTruncated, out.Exchanges[len(out.Exchanges)-1].TextTruncated)
	}
	if last := out.Elisions[len(out.Elisions)-1]; last.Kind != TranscriptElisionPromptText {
		t.Fatalf("elisions = %+v", out.Elisions)
	}
}

// Past that the oldest exchanges go, so the result is bounded, but the
// newest is always kept.
func TestFitTranscriptDropsTheOldestExchangesLast(t *testing.T) {
	t.Parallel()
	in := bigTranscript(6)
	measure := transcriptSize(false)
	full := measure(in)
	// Room for about three truncated exchanges.
	out, fits := FitTranscript(in, 3*(handoffPromptCap+1300), false, measure)
	if !fits || len(out.Exchanges) >= len(in.Exchanges) || len(out.Exchanges) == 0 {
		t.Fatalf("fits=%v, %d of %d exchanges remain (untrimmed size %d)", fits, len(out.Exchanges), len(in.Exchanges), full)
	}
	dropped := len(in.Exchanges) - len(out.Exchanges)
	last := out.Elisions[len(out.Elisions)-1]
	if last.Kind != TranscriptElisionOldestExchanges || last.Count != dropped || last.First != 1 || last.Last != dropped {
		t.Fatalf("elisions = %+v, dropped %d", out.Elisions, dropped)
	}
	if !strings.HasPrefix(out.Exchanges[len(out.Exchanges)-1].Text, "prompt 5 ") {
		t.Fatalf("the newest exchange was not kept")
	}
	if !strings.Contains(DescribeTranscriptElisions(out.Elisions), fmt.Sprintf("the oldest %d exchanges dropped", dropped)) {
		t.Fatalf("description: %s", DescribeTranscriptElisions(out.Elisions))
	}

	// A limit nothing can meet keeps the newest exchange and says it did not fit.
	out, fits = FitTranscript(in, 1, false, measure)
	if fits || len(out.Exchanges) != 1 || !strings.HasPrefix(out.Exchanges[0].Text, "prompt 5 ") {
		t.Fatalf("fits=%v, exchanges=%d", fits, len(out.Exchanges))
	}
}

func TestFitTranscriptCollapsesToolCallsByName(t *testing.T) {
	t.Parallel()
	in := bigTranscript(6)
	measure := transcriptSize(false)
	out, _ := FitTranscript(in, measure(in)*9/10, false, measure)
	var collapsed []TranscriptStep
	for _, s := range out.Exchanges[0].Steps {
		if s.Kind == TranscriptStepCollapsed {
			collapsed = append(collapsed, s)
		}
		if s.Kind == TranscriptStepTool {
			t.Fatalf("a tool call survived in a collapsed exchange: %+v", s)
		}
	}
	if len(collapsed) != 1 || collapsed[0].Text != "3 tool calls: Bash ×2, Read ×1" {
		t.Fatalf("collapsed = %+v", collapsed)
	}
}

func TestFitTranscriptDoesNotModifyItsInput(t *testing.T) {
	t.Parallel()
	in := bigTranscript(6)
	before := fmt.Sprintf("%+v", in)
	beforeTool := *in.Exchanges[0].Steps[1].Tool
	FitTranscript(in, 2000, true, transcriptSize(true))
	if fmt.Sprintf("%+v", in) != before || *in.Exchanges[0].Steps[1].Tool != beforeTool {
		t.Fatalf("FitTranscript changed its input")
	}
}

func TestFitTranscriptKeepsSlashCommandReplies(t *testing.T) {
	t.Parallel()
	// Older than the protected tail.
	in := bigTranscript(5)
	in.Exchanges[0].Steps = []TranscriptStep{
		{Kind: TranscriptStepCommand, Text: "/model", Output: "Set model to opus"},
		{Kind: TranscriptStepShell, Text: "ls", Output: strings.Repeat("x", 4000)},
	}
	measure := func(t Transcript) int {
		return len(t.Exchanges[0].Steps[0].Output) + len(t.Exchanges[0].Steps[1].Output)
	}
	out, fits := FitTranscript(in, 100, true, measure)
	if !fits || out.Exchanges[0].Steps[0].Output != "Set model to opus" || out.Exchanges[0].Steps[1].Output != "" {
		t.Fatalf("out = %+v fits=%v", out.Exchanges[0].Steps, fits)
	}
}

func TestSmallestFitFindsTheSmallestPrefix(t *testing.T) {
	t.Parallel()
	for n := range 12 {
		for k := 1; k <= n; k++ {
			calls := 0
			// apply(j) has size n-j, so a limit of n-k first fits at j == k.
			got, fits := SmallestFit(n, n-k, func(size int) int { calls++; return size }, func(j int) int { return n - j })
			if !fits || got != n-k {
				t.Fatalf("n=%d k=%d: got %d fits=%v", n, k, got, fits)
			}
			if calls > 6 {
				t.Fatalf("n=%d: %d measurements for a binary search", n, calls)
			}
		}
		if got, fits := SmallestFit(n, -1, func(size int) int { return size }, func(j int) int { return n - j }); fits || got != 0 {
			t.Fatalf("n=%d: unfittable gave %d fits=%v, want apply(n) and false", n, got, fits)
		}
	}
}

func TestDescribeTranscriptElisions(t *testing.T) {
	t.Parallel()
	got := DescribeTranscriptElisions([]TranscriptElision{
		{Kind: TranscriptElisionToolOutput, First: 1, Last: 4, Count: 12},
		{Kind: TranscriptElisionToolCalls, First: 2, Last: 2, Count: 3},
		{Kind: TranscriptElisionAssistantText, First: 1, Last: 9, Count: 8},
		{Kind: TranscriptElisionPromptText, First: 1, Last: 1, Count: 1},
	})
	want := "output of 12 tool calls or commands in exchanges 1–4; 3 tool calls in exchange 2 collapsed to counts; 8 agent messages in exchanges 1–9 shortened; 1 long prompts in exchange 1 truncated"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}
