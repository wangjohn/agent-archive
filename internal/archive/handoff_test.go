package archive

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateHandoffGolden = flag.Bool("update", false, "rewrite internal/archive/testdata/handoff golden files")

// handoffBundle filters a handoff fixture through the harness's adapter, the
// same path the collector and `handoff --source local` take.
func handoffBundle(t *testing.T, harness string) SourceBundle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "handoff", harness+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAdapter(harness)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := adapter.FilterJSONL(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: adapter.Name()}
	bundle, err := NewSourceBundle(reg, adapter, filtered, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func markdownSize(h Handoff) int {
	return len(RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true}))
}

// Each harness renders to a checked-in golden document. Regenerate with
// `go test ./internal/archive -run TestHandoffGolden -update` and review the
// diff.
func TestHandoffGolden(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "cursor"} {
		t.Run(harness, func(t *testing.T) {
			h, err := BuildHandoff(handoffBundle(t, harness), nil, HandoffOptions{Source: "local"})
			if err != nil {
				t.Fatal(err)
			}
			got := RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true})
			golden := filepath.Join("testdata", "handoff", harness+".md")
			if *updateHandoffGolden {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("handoff for %s differs from %s:\n%s", harness, golden, got)
			}
		})
	}
}

func TestHandoffClaudeContent(t *testing.T) {
	h, err := BuildHandoff(handoffBundle(t, "claude"), nil, HandoffOptions{Source: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Exchanges) != 2 {
		t.Fatalf("exchanges = %d, want 2 (the task notification is not a prompt)", len(h.Exchanges))
	}
	if h.Exchanges[0].Prompt != "Why does the widget test fail?" {
		t.Fatalf("first prompt = %q (injected reminder must be stripped)", h.Exchanges[0].Prompt)
	}
	if strings.Contains(h.Exchanges[1].Prompt, "sk-abcdefghijklmnopqrstuv") || !strings.Contains(h.Exchanges[1].Prompt, "[REDACTED]") {
		t.Fatalf("credential not redacted: %q", h.Exchanges[1].Prompt)
	}
	if h.Workspace != (HandoffWorkspace{Directory: "widgets", Branch: "fix/widget-test"}) {
		t.Fatalf("workspace = %#v", h.Workspace)
	}
	if got := strings.Join(h.FilesTouched, ","); got != "widget/size.go" {
		t.Fatalf("files touched = %q", got)
	}
	if len(h.Plan) != 3 || h.Plan[1].Status != "in_progress" {
		t.Fatalf("plan = %#v", h.Plan)
	}
	if h.LeftOff != "Tests pass now. Next: add a regression test for Size." {
		t.Fatalf("left off = %q", h.LeftOff)
	}
	var bash, edit *HandoffToolCall
	for _, step := range h.Exchanges[0].Steps {
		if step.Tool != nil && step.Tool.Name == "Bash" {
			bash = step.Tool
		}
		if step.Tool != nil && step.Tool.Name == "Edit" {
			edit = step.Tool
		}
	}
	if bash == nil || !bash.IsError || bash.Summary != "go test ./widget/..." || !strings.Contains(bash.Result, "size = 3, want 4") {
		t.Fatalf("bash call = %#v", bash)
	}
	rendered := string(RenderHandoffMarkdown(h, HandoffRenderOptions{Preamble: true}))
	if edit == nil || strings.Contains(rendered, "len(w.parts)\"") || strings.Contains(rendered, "new_string") {
		t.Fatalf("edit body leaked or edit missing: %#v\n%s", edit, rendered)
	}
}

func TestHandoffCodexContent(t *testing.T) {
	h, err := BuildHandoff(handoffBundle(t, "codex"), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Exchanges) != 1 || h.Exchanges[0].Prompt != "Rename Widget.parts to Widget.pieces everywhere." {
		t.Fatalf("exchanges = %#v", h.Exchanges)
	}
	var names []string
	for _, step := range h.Exchanges[0].Steps {
		if step.Tool != nil {
			names = append(names, step.Tool.Name+":"+step.Tool.Summary)
			if step.Tool.Name == "exec" && !strings.Contains(step.Tool.Result, "widget/widget.go:4") {
				t.Fatalf("list-shaped exec output missing: %#v", step.Tool)
			}
		}
	}
	want := "exec:rg -n 'parts' widget|apply_patch:widget/size.go, widget/widget.go|update_plan:updated the plan"
	if got := strings.Join(names, "|"); got != want {
		t.Fatalf("tool steps = %q, want %q", got, want)
	}
	if got := strings.Join(h.FilesTouched, ","); got != "widget/size.go,widget/widget.go" {
		t.Fatalf("files touched = %q", got)
	}
	if len(h.Plan) != 2 || h.Plan[0].Status != "completed" {
		t.Fatalf("plan = %#v", h.Plan)
	}
	if len(h.Session.Models) != 1 || h.Session.Models[0] != "gpt-6-astra" {
		t.Fatalf("models = %#v", h.Session.Models)
	}
}

func TestHandoffCursorContent(t *testing.T) {
	h, err := BuildHandoff(handoffBundle(t, "cursor"), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.ToolResultsUnavailable {
		t.Fatal("Cursor records no tool results; the handoff must say so")
	}
	if len(h.Exchanges) != 1 || h.Exchanges[0].Prompt != "Tighten the intro paragraph of the megafauna post." {
		t.Fatalf("prompt wrapper not removed: %#v", h.Exchanges)
	}
	rendered := string(RenderHandoffMarkdown(h, HandoffRenderOptions{}))
	if strings.Contains(rendered, "Long ago, there were many") {
		t.Fatalf("StrReplace body leaked:\n%s", rendered)
	}
	if strings.Contains(rendered, "picking up work") {
		t.Fatalf("preamble rendered with Preamble false:\n%s", rendered)
	}
}

// bigHandoff builds n exchanges, each with a long prompt, long assistant
// text, and three tool calls with long results.
func bigHandoff(n int) Handoff {
	h := Handoff{Version: HandoffVersion, Session: HandoffSession{Harness: "claude"}, LeftOff: "final words"}
	for i := 0; i < n; i++ {
		exchange := HandoffExchange{Prompt: fmt.Sprintf("prompt %d ", i) + strings.Repeat("p", 3000)}
		exchange.Steps = append(exchange.Steps, HandoffStep{Kind: "text", Text: strings.Repeat("a", 2000)})
		for j := 0; j < 3; j++ {
			exchange.Steps = append(exchange.Steps, HandoffStep{Kind: "tool", Tool: &HandoffToolCall{Name: "Bash", Summary: "go test", Result: strings.Repeat("r", 1500), ResultLines: 1, ResultBytes: 1500}})
		}
		h.Exchanges = append(h.Exchanges, exchange)
	}
	return h
}

func TestFitHandoffAppliesStepsInOrderAndKeepsRecentExchanges(t *testing.T) {
	h := bigHandoff(40)
	full := markdownSize(h)
	fit, ok := FitHandoff(h, 200_000, markdownSize)
	if !ok || markdownSize(fit) > 200_000 {
		t.Fatalf("fit = %v, size %d of %d", ok, markdownSize(fit), full)
	}
	if len(fit.Exchanges) != 40 {
		t.Fatalf("exchanges dropped: %d", len(fit.Exchanges))
	}
	for i, exchange := range fit.Exchanges {
		if !strings.HasPrefix(exchange.Prompt, fmt.Sprintf("prompt %d ", i)) {
			t.Fatalf("prompt %d lost", i)
		}
	}
	for _, exchange := range fit.Exchanges[len(fit.Exchanges)-handoffKeptExchanges:] {
		for _, step := range exchange.Steps {
			if step.Tool != nil && (step.Tool.Result == "" || step.Tool.ResultOmitted) {
				t.Fatal("a recent exchange lost its tool output")
			}
			if step.TextTruncated {
				t.Fatal("a recent exchange lost its assistant text")
			}
		}
	}
	if len(fit.Elisions) == 0 || fit.Elisions[0].Kind != HandoffElisionToolOutput || fit.Elisions[0].First != 1 {
		t.Fatalf("elisions = %#v", fit.Elisions)
	}
	// FitHandoff must not modify its input.
	if markdownSize(h) != full || h.Exchanges[0].Steps[1].Tool.Result == "" {
		t.Fatal("FitHandoff mutated its input")
	}
}

func TestFitHandoffRunsEveryStepUnderPressure(t *testing.T) {
	fit, _ := FitHandoff(bigHandoff(40), 40_000, markdownSize)
	var kinds []string
	for _, e := range fit.Elisions {
		kinds = append(kinds, e.Kind)
	}
	want := []string{HandoffElisionToolOutput, HandoffElisionToolCalls, HandoffElisionAssistantText, HandoffElisionPromptText}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("elision order = %v, want %v", kinds, want)
	}
	if fit.Exchanges[0].CollapsedTools != "3 tool calls: Bash ×3" {
		t.Fatalf("collapsed = %q", fit.Exchanges[0].CollapsedTools)
	}
	for _, exchange := range fit.Exchanges {
		if exchange.Prompt == "" {
			t.Fatal("a prompt was dropped")
		}
	}
	if fit.LeftOff != "final words" {
		t.Fatal("left off was trimmed")
	}
}

func TestFitHandoffReportsWhenItCannotFit(t *testing.T) {
	fit, ok := FitHandoff(bigHandoff(5), 1_000, markdownSize)
	if ok {
		t.Fatalf("fit reported success at %d bytes", markdownSize(fit))
	}
	if len(fit.Exchanges) != 5 {
		t.Fatal("exchanges dropped")
	}
}

func TestFitHandoffWithinBudgetIsUnchanged(t *testing.T) {
	h := bigHandoff(2)
	fit, ok := FitHandoff(h, 0, markdownSize)
	if !ok || len(fit.Elisions) != 0 || markdownSize(fit) != markdownSize(h) {
		t.Fatal("no budget must mean no trimming")
	}
	fit, ok = FitHandoff(h, 1<<30, markdownSize)
	if !ok || len(fit.Elisions) != 0 {
		t.Fatal("a generous budget must mean no trimming")
	}
}

// The footer names the saved full record only when something was trimmed.
func TestHandoffFooterNamesFullRecordOnlyWhenTrimmed(t *testing.T) {
	h := bigHandoff(10)
	h.FullRecordPath = "/data/handoffs/x.md"
	if strings.Contains(string(RenderHandoffMarkdown(h, HandoffRenderOptions{})), "Full record") {
		t.Fatal("full record named on an untrimmed handoff")
	}
	fit, _ := FitHandoff(h, 20_000, markdownSize)
	if !strings.Contains(string(RenderHandoffMarkdown(fit, HandoffRenderOptions{})), "Full record: /data/handoffs/x.md") {
		t.Fatal("trimmed handoff does not name its full record")
	}
}

func TestTrimResultKeepsHeadAndTail(t *testing.T) {
	var lines []string
	for i := 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	got := trimResult(strings.Join(lines, "\n"), 4, 10_000)
	want := "line 1\nline 2\n… 46 lines omitted …\nline 49\nline 50"
	if got != want {
		t.Fatalf("got %q", got)
	}
	long := trimResult(strings.Repeat("é", 3000), 12, 100)
	if !strings.Contains(long, "bytes omitted") || !json.Valid([]byte(fmt.Sprintf("%q", long))) || len(long) > 200 {
		t.Fatalf("byte cap: %d bytes %q", len(long), long)
	}
	for _, r := range long {
		if r == '\uFFFD' {
			t.Fatal("a character was split")
		}
	}
}

func TestCodeFenceOutrunsBackticksInContent(t *testing.T) {
	if got := codeFence("plain"); got != "```" {
		t.Fatalf("got %q", got)
	}
	if got := codeFence("has ```` inside"); got != "`````" {
		t.Fatalf("got %q", got)
	}
}

func TestHandoffJSONRoundTrips(t *testing.T) {
	h, err := BuildHandoff(handoffBundle(t, "codex"), nil, HandoffOptions{Source: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back Handoff
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(RenderHandoffMarkdown(back, HandoffRenderOptions{}), RenderHandoffMarkdown(h, HandoffRenderOptions{})) {
		t.Fatal("JSON round trip changed the rendering")
	}
	if back.Version != HandoffVersion || back.Session.Source != "archive" {
		t.Fatalf("session = %#v", back.Session)
	}
}
