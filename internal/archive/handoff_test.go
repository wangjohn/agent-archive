package archive

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	for i := range n {
		exchange := HandoffExchange{Prompt: fmt.Sprintf("prompt %d ", i) + strings.Repeat("p", 3000)}
		exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepText, Text: strings.Repeat("a", 2000)})
		for range 3 {
			exchange.Steps = append(exchange.Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "Bash", Summary: "go test", Result: strings.Repeat("r", 1500), ResultLines: 1, ResultBytes: 1500}})
		}
		h.Exchanges = append(h.Exchanges, exchange)
	}
	return h
}

func TestFitHandoffAppliesStepsInOrderAndKeepsRecentExchanges(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	fit, _ := FitHandoff(bigHandoff(40), 40_000, markdownSize)
	var kinds []HandoffElisionKind
	for _, e := range fit.Elisions {
		kinds = append(kinds, e.Kind)
	}
	want := []HandoffElisionKind{HandoffElisionToolOutput, HandoffElisionToolCalls, HandoffElisionAssistantText, HandoffElisionPromptText}
	if !slices.Equal(kinds, want) {
		t.Fatalf("elision order = %v, want %v", kinds, want)
	}
	if steps := fit.Exchanges[0].Steps; len(steps) != 2 || steps[1].Kind != HandoffStepCollapsed || steps[1].Text != "3 tool calls: `Bash` ×3" {
		t.Fatalf("collapsed = %#v", steps)
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
	t.Parallel()
	fit, ok := FitHandoff(bigHandoff(5), 1_000, markdownSize)
	if ok {
		t.Fatalf("fit reported success at %d bytes", markdownSize(fit))
	}
	if len(fit.Exchanges) != 5 {
		t.Fatal("exchanges dropped")
	}
}

func TestFitHandoffWithinBudgetIsUnchanged(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	if got := codeFence("plain"); got != "```" {
		t.Fatalf("got %q", got)
	}
	if got := codeFence("has ```` inside"); got != "`````" {
		t.Fatalf("got %q", got)
	}
}

func TestHandoffJSONRoundTrips(t *testing.T) {
	t.Parallel()
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

// One long autonomous exchange is still trimmed: only its last
// handoffKeptSteps steps are protected.
func TestFitHandoffTrimsASingleLongExchange(t *testing.T) {
	t.Parallel()
	h := bigHandoff(1)
	for i := range 60 {
		h.Exchanges[0].Steps = append(h.Exchanges[0].Steps, HandoffStep{Kind: HandoffStepTool, Tool: &HandoffToolCall{Name: "Read", Summary: strconv.Itoa(i), Result: strings.Repeat("r", 1500)}})
	}
	fit, ok := FitHandoff(h, 40_000, markdownSize)
	if !ok {
		t.Fatalf("a single exchange could not be fitted: %d bytes", markdownSize(fit))
	}
	steps := fit.Exchanges[0].Steps
	tail := steps[len(steps)-handoffKeptSteps:]
	for _, step := range tail {
		if step.Tool == nil || step.Tool.Result == "" {
			t.Fatalf("a protected step was trimmed: %#v", step)
		}
	}
	if steps[1].Tool == nil || !steps[1].Tool.ResultOmitted {
		t.Fatalf("an early step kept its output: %#v", steps[1])
	}
}

func TestProtectedStart(t *testing.T) {
	t.Parallel()
	ex := func(n int) HandoffExchange { return HandoffExchange{Steps: make([]HandoffStep, n)} }
	got := protectedStart([]HandoffExchange{ex(5), ex(5), ex(4), ex(30), ex(2)}, 3, 20)
	want := []int{5, 5, 4, 12, 0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// testBundle wraps native records in a filtered-shape bundle for one harness.
func testBundle(harness string, records ...map[string]any) SourceBundle {
	return SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture:       SourceCapture{Harness: Harness{Name: harness}, AdapterName: harness, AdapterVersion: adapterVersion, SourceFormat: harness + "-jsonl", FilterVersion: FilterVersion, CapturedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)},
		NativeRecords: records,
	}
}

func toolSteps(h Handoff) []*HandoffToolCall {
	var out []*HandoffToolCall
	for _, exchange := range h.Exchanges {
		for _, step := range exchange.Steps {
			if step.Tool != nil {
				out = append(out, step.Tool)
			}
		}
	}
	return out
}

// Calls and results without ids in one record are paired by position, each
// with its own raw item and its own output — not all with the first.
func TestHandoffPairsUnidentifiedCallsInOneRecordByPosition(t *testing.T) {
	t.Parallel()
	bundle := testBundle("cursor",
		map[string]any{"role": "user", "message": map[string]any{"content": "go"}},
		map[string]any{"role": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "exec", "input": "ls widget"},
			map[string]any{"type": "tool_use", "name": "exec", "input": "go vet ./widget"},
		}}},
		map[string]any{"role": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "content": "size.go"},
			map[string]any{"type": "tool_result", "content": "vet: clean"},
		}}},
	)
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tools := toolSteps(h)
	if len(tools) != 2 || tools[0].Summary != "ls widget" || tools[1].Summary != "go vet ./widget" {
		t.Fatalf("summaries = %#v", tools)
	}
	if tools[0].Result != "size.go" || tools[1].Result != "vet: clean" {
		t.Fatalf("results = %q, %q", tools[0].Result, tools[1].Result)
	}
}

// The directory is where the session started; a later `cd` recorded in cwd
// must not make paths relative to a subdirectory.
func TestHandoffUsesTheStartingDirectory(t *testing.T) {
	t.Parallel()
	record := func(cwd string, content any) map[string]any {
		return map[string]any{"type": "assistant", "cwd": cwd, "message": map[string]any{"role": "assistant", "content": content}}
	}
	bundle := testBundle("claude",
		map[string]any{"type": "user", "cwd": "/repo", "message": map[string]any{"role": "user", "content": "fix it"}},
		record("/repo", []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Edit", "input": map[string]any{"file_path": "/repo/docs/a.md"}}}),
		record("/repo/internal/archive", []any{map[string]any{"type": "tool_use", "id": "t2", "name": "Edit", "input": map[string]any{"file_path": "/repo/internal/archive/views.go"}}}),
	)
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Workspace.Directory != "repo" || strings.Join(h.FilesTouched, ",") != "docs/a.md,internal/archive/views.go" {
		t.Fatalf("workspace = %#v files = %v", h.Workspace, h.FilesTouched)
	}
}

// A plan call whose items cannot be read leaves the earlier plan; an explicit
// empty list clears it.
func TestHandoffPlanSurvivesAnUnreadablePlanCall(t *testing.T) {
	t.Parallel()
	plan := func(id string, input map[string]any) map[string]any {
		return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "TodoWrite", "input": input}}}}
	}
	prompt := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "go"}}
	first := plan("p1", map[string]any{"todos": []any{map[string]any{"content": "Ship it", "status": "pending"}}})
	h, err := BuildHandoff(testBundle("claude", prompt, first, plan("p2", map[string]any{"unknown": true})), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Plan) != 1 || h.Plan[0].Text != "Ship it" {
		t.Fatalf("plan erased: %#v", h.Plan)
	}
	h, err = BuildHandoff(testBundle("claude", prompt, first, plan("p3", map[string]any{"todos": []any{}})), nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Plan) != 0 {
		t.Fatalf("explicit empty plan did not clear: %#v", h.Plan)
	}
}

// With no timestamps in the records, times come only from the caller or from
// published metadata — never from the moment the bundle was built.
func TestHandoffDoesNotInventActivityTimes(t *testing.T) {
	t.Parallel()
	bundle := testBundle("cursor", map[string]any{"role": "user", "message": map[string]any{"content": "hello"}})
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Session.StartedAt != nil || h.Session.LastActivityAt != nil {
		t.Fatalf("invented times: %v %v", h.Session.StartedAt, h.Session.LastActivityAt)
	}
	mtime := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	h, _ = BuildHandoff(bundle, nil, HandoffOptions{LastActivityAt: mtime})
	if h.Session.LastActivityAt == nil || !h.Session.LastActivityAt.Equal(mtime) {
		t.Fatalf("caller's time not used: %v", h.Session.LastActivityAt)
	}
	captured := time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)
	h, _ = BuildHandoff(bundle, &Metadata{CapturedAt: captured}, HandoffOptions{LastActivityAt: mtime})
	if !h.Session.LastActivityAt.Equal(captured) {
		t.Fatalf("published capture time not preferred: %v", h.Session.LastActivityAt)
	}
}

func TestCleanPromptRestoresSlashCommands(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"<command-message>review-pr</command-message>\n<command-name>/review-pr</command-name>\n<command-args>12</command-args>": "/review-pr 12",
		"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>":           "/model",
		"<timestamp>Mon</timestamp>\n<user_query>\nhi\n</user_query>":                                                            "hi",
		"please explain <command-name> tags": "please explain <command-name> tags",
	} {
		if got := cleanPrompt(in); got != want {
			t.Errorf("cleanPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

// A Cursor text transcript has role sections instead of records; it still
// renders its exchanges.
func TestHandoffRendersCursorTextTranscripts(t *testing.T) {
	t.Parallel()
	filtered, err := CursorAdapter{}.FilterText(strings.NewReader("user: Tighten the intro.\nassistant: Reading it.\ntool: intro.md: 3 lines\nassistant: Done; the first sentence is shorter.\nuser: thanks\n"), time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Exchanges) != 2 || h.Exchanges[0].Prompt != "Tighten the intro." || h.Exchanges[1].Prompt != "thanks" {
		t.Fatalf("exchanges = %#v", h.Exchanges)
	}
	if steps := h.Exchanges[0].Steps; len(steps) != 3 || steps[1].Tool == nil || steps[1].Tool.Result != "intro.md: 3 lines" {
		t.Fatalf("steps = %#v", steps)
	}
	if h.LeftOff != "Done; the first sentence is shorter." {
		t.Fatalf("left off = %q", h.LeftOff)
	}
}

// Fitting a large session takes a handful of measurements per step, not one
// per exchange.
func TestFitHandoffMeasuresLogarithmically(t *testing.T) {
	t.Parallel()
	h := bigHandoff(512)
	calls := 0
	fit, ok := FitHandoff(h, 2_000_000, func(x Handoff) int { calls++; return markdownSize(x) })
	if !ok {
		t.Fatalf("did not fit: %d", markdownSize(fit))
	}
	if calls > 4*(2+10)+1 {
		t.Fatalf("%d measurements for 512 exchanges", calls)
	}
	// Trimming stops partway through the conversation rather than applying
	// the last step to every exchange.
	e := fit.Elisions[len(fit.Elisions)-1]
	if e.First != 1 || e.Last >= 512-handoffKeptExchanges {
		t.Fatalf("elision = %#v", e)
	}
}

// A compaction summary is shown where it happened: after /compact the agent
// worked from it, not from the turns before it.
func TestHandoffShowsCompactionSummaries(t *testing.T) {
	t.Parallel()
	filtered, err := ClaudeAdapter{}.FilterJSONL(bytes.NewReader(fixture(t, "claude-compaction.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, reg.SessionStartedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, err := BuildHandoff(bundle, nil, HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var summaries int
	for _, exchange := range h.Exchanges {
		for _, step := range exchange.Steps {
			if step.Kind == HandoffStepSummary && step.Text != "" {
				summaries++
			}
		}
		if strings.Contains(exchange.Prompt, "This session is being continued") {
			t.Fatalf("compaction summary rendered as a prompt: %q", exchange.Prompt)
		}
	}
	if summaries == 0 {
		t.Fatalf("no compaction summary in the handoff: %#v", h.Exchanges)
	}
	if !strings.Contains(string(RenderHandoffMarkdown(h, HandoffRenderOptions{})), "**Conversation compacted.**") {
		t.Fatal("summary not rendered")
	}
}
