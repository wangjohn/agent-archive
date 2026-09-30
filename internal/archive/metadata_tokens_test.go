package archive

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func intp(n int) *int { return &n }

// tokenFields lists, for one Counts and one ModelTokens, the same five
// token fields side by side so a test can compare them one by one.
type tokenField struct {
	name  string
	total *int
	model func(ModelTokens) *int
}

func tokenFieldsOf(c Counts) []tokenField {
	return []tokenField{
		{"input", c.InputTokens, func(m ModelTokens) *int { return m.InputTokens }},
		{"output", c.OutputTokens, func(m ModelTokens) *int { return m.OutputTokens }},
		{"cache read", c.CacheReadTokens, func(m ModelTokens) *int { return m.CacheReadTokens }},
		{"cache write", c.CacheWriteTokens, func(m ModelTokens) *int { return m.CacheWriteTokens }},
		{"reasoning", c.ReasoningTokens, func(m ModelTokens) *int { return m.ReasoningTokens }},
	}
}

// assertModelTokensSum is the sum-consistency invariant: each session-wide
// token count is the sum of that count over the per-model entries, and is nil
// exactly when every entry's is. It holds below the 2^53 saturation.
func assertModelTokensSum(t *testing.T, label string, m Metadata) {
	t.Helper()
	seen := map[string]bool{}
	for _, entry := range m.ModelTokens {
		if entry.Model == "" {
			t.Errorf("%s: a model_tokens entry has no model: %#v", label, entry)
		}
		if seen[entry.Model] {
			t.Errorf("%s: model %q appears twice in model_tokens", label, entry.Model)
		}
		seen[entry.Model] = true
	}
	for _, field := range tokenFieldsOf(m.Counts) {
		sum, present := 0, false
		for _, entry := range m.ModelTokens {
			if value := field.model(entry); value != nil {
				sum, present = sum+*value, true
			}
		}
		switch {
		case field.total == nil && present:
			t.Errorf("%s: %s tokens are unknown but per-model entries sum to %d", label, field.name, sum)
		case field.total != nil && !present:
			t.Errorf("%s: %s tokens are %d but no model reports them", label, field.name, *field.total)
		case field.total != nil && *field.total != sum:
			t.Errorf("%s: %s tokens are %d but per-model entries sum to %d", label, field.name, *field.total, sum)
		}
	}
}

// Claude Code streams one message as several records repeating its usage:
// each message counts once, under its own model. A record naming no model is
// counted under "unknown", and a <synthetic> message counts nowhere.
func TestModelTokensClaudeMultiModelStreamed(t *testing.T) {
	t.Parallel()
	_, metadata := parsedFixture(t, "claude", "claude-model-tokens.jsonl")
	want := []ModelTokens{
		{Model: "claude-opus-synthetic", InputTokens: intp(107), OutputTokens: intp(69), CacheReadTokens: intp(1000), CacheWriteTokens: intp(211), ReasoningTokens: intp(40)},
		{Model: "claude-sonnet-synthetic", InputTokens: intp(30), OutputTokens: intp(20), CacheReadTokens: intp(300), ReasoningTokens: intp(5)},
		{Model: UnknownModel, InputTokens: intp(3), OutputTokens: intp(2)},
	}
	if !reflect.DeepEqual(metadata.ModelTokens, want) {
		t.Fatalf("model tokens = %s\nwant %s", asJSON(metadata.ModelTokens), asJSON(want))
	}
	countIs(t, "input tokens", metadata.Counts.InputTokens, 140)
	countIs(t, "output tokens", metadata.Counts.OutputTokens, 91)
	countIs(t, "cache read tokens", metadata.Counts.CacheReadTokens, 1300)
	countIs(t, "cache write tokens", metadata.Counts.CacheWriteTokens, 211)
	countIs(t, "reasoning tokens", metadata.Counts.ReasoningTokens, 45)
	assertModelTokensSum(t, "claude-model-tokens", metadata)
}

func TestToolErrorsAndMCPCallsFromClaudeFixture(t *testing.T) {
	t.Parallel()
	_, metadata := parsedFixture(t, "claude", "claude-model-tokens.jsonl")
	countIs(t, "tool results", metadata.Counts.ToolResults, 4)
	countIs(t, "tool errors", metadata.Counts.ToolErrors, 2)
	want := []ToolUsage{{Name: "github", Count: 2}, {Name: "linear", Count: 1}}
	if !reflect.DeepEqual(metadata.MCPCalls, want) {
		t.Fatalf("mcp calls = %v, want %v", metadata.MCPCalls, want)
	}
	// MCP tools are still ordinary tools_used entries.
	found := false
	for _, tool := range metadata.ToolsUsed {
		found = found || tool.Name == "mcp__github__create_issue"
	}
	if !found {
		t.Fatalf("tools used = %v", metadata.ToolsUsed)
	}
}

// Codex assigns the model per turn (turn_context), not per token record: a
// model switched mid-session splits the counts at the switch. Its cache writes
// come from cache_write_input_tokens.
func TestModelTokensCodexModelSwitch(t *testing.T) {
	t.Parallel()
	_, metadata := parsedFixture(t, "codex", "codex-model-switch.jsonl")
	want := []ModelTokens{
		{Model: "gpt-synthetic-a", InputTokens: intp(2200), OutputTokens: intp(130), CacheReadTokens: intp(1600), CacheWriteTokens: intp(50), ReasoningTokens: intp(40)},
		{Model: "gpt-synthetic-b", InputTokens: intp(500), OutputTokens: intp(25), CacheReadTokens: intp(100), CacheWriteTokens: intp(7), ReasoningTokens: intp(0)},
	}
	if !reflect.DeepEqual(metadata.ModelTokens, want) {
		t.Fatalf("model tokens = %s\nwant %s", asJSON(metadata.ModelTokens), asJSON(want))
	}
	// Codex's own thread total for this session: 2700 / 155 / 1700 / 57 / 40.
	countIs(t, "input tokens", metadata.Counts.InputTokens, 2700)
	countIs(t, "output tokens", metadata.Counts.OutputTokens, 155)
	countIs(t, "cache read tokens", metadata.Counts.CacheReadTokens, 1700)
	countIs(t, "cache write tokens", metadata.Counts.CacheWriteTokens, 57)
	countIs(t, "reasoning tokens", metadata.Counts.ReasoningTokens, 40)
	assertModelTokensSum(t, "codex-model-switch", metadata)
	if metadata.Counts.ToolErrors != nil {
		t.Errorf("Codex writes no is_error flag, so tool errors are unknown, got %d", *metadata.Counts.ToolErrors)
	}
}

// A Codex token record before any turn_context has no model to be counted
// under.
func TestModelTokensCodexRecordBeforeTurnContextIsUnknown(t *testing.T) {
	t.Parallel()
	adapter := CodexAdapter{}
	filtered, err := adapter.FilterJSONL(strings.NewReader(
		`{"type":"token_usage_record","timestamp":"2026-09-20T11:00:05Z","payload":{"turn_token_usage":{"input_tokens":5,"output_tokens":2}}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := parserTestMetadata(t, parserTestBundle(t, "codex", adapter, filtered))
	want := []ModelTokens{{Model: UnknownModel, InputTokens: intp(5), OutputTokens: intp(2)}}
	if !reflect.DeepEqual(metadata.ModelTokens, want) {
		t.Fatalf("model tokens = %s", asJSON(metadata.ModelTokens))
	}
	assertModelTokensSum(t, "codex-no-turn-context", metadata)
}

// Sessions with no token accounting publish no model_tokens, and Cursor's
// unmetered zeros (which the composer filter refuses to keep as observed
// zeros) leave every count unknown rather than 0.
func TestModelTokensAbsentWithoutAccounting(t *testing.T) {
	t.Parallel()
	_, metadata := parsedFixture(t, "cursor", "cursor-turn.jsonl")
	if metadata.ModelTokens != nil || metadata.Counts.InputTokens != nil || metadata.Counts.ReasoningTokens != nil {
		t.Fatalf("cursor without tokens: %#v %s", metadata.Counts, asJSON(metadata.ModelTokens))
	}
	countIs(t, "cursor tool errors", metadata.Counts.ToolErrors, 0)

	composer := CursorComposer{
		Composer: json.RawMessage(`{"_v":18,"composerId":"c","fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2}]}`),
		Bubbles: []CursorBubble{
			{ID: "b1", Value: json.RawMessage(`{"_v":3,"bubbleId":"b1","type":1,"text":"hello","createdAt":"2026-09-21T14:13:21Z","tokenCount":{"inputTokens":0,"outputTokens":0}}`)},
			{ID: "b2", Value: json.RawMessage(`{"_v":3,"bubbleId":"b2","type":2,"text":"hi","createdAt":"2026-09-21T14:13:22Z","modelInfo":{"modelName":"synthetic-model-a"},"tokenCount":{"inputTokens":0,"outputTokens":0}}`)},
		},
	}
	filtered, err := (CursorAdapter{}).FilterComposer(composer)
	if err != nil {
		t.Fatal(err)
	}
	metadata = parserTestMetadata(t, parserTestBundle(t, "cursor", CursorAdapter{}, filtered))
	counts := metadata.Counts
	if metadata.ModelTokens != nil || counts.InputTokens != nil || counts.OutputTokens != nil || counts.CacheReadTokens != nil || counts.CacheWriteTokens != nil || counts.ReasoningTokens != nil {
		t.Fatalf("all-zero Cursor counts became observed zeros: %#v %s", counts, asJSON(metadata.ModelTokens))
	}
}

// A Cursor chat's metered messages split by the model each names.
func TestModelTokensCursorComposerChat(t *testing.T) {
	t.Parallel()
	filtered, _ := filterComposerFixture(t)
	bundle, err := NewSourceBundle(registrationFor("cursor"), CursorAdapter{}, filtered, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	metadata := parserTestMetadata(t, bundle)
	if len(metadata.ModelTokens) == 0 {
		t.Fatalf("no model tokens: %#v", metadata.Counts)
	}
	assertModelTokensSum(t, "cursor-composer", metadata)
}

func registrationFor(harness string) SessionRegistration {
	reg := registration()
	reg.Harness = Harness{Name: harness}
	return reg
}

// The invariant holds for every fixture the package has, not only the ones
// built for it.
func TestModelTokensSumMatchesCountsOnEveryFixture(t *testing.T) {
	t.Parallel()
	bundles := fixtureBundles(t)
	if len(bundles) == 0 {
		t.Fatal("no fixtures")
	}
	withTokens := 0
	for name, bundle := range bundles {
		metadata := parserTestMetadata(t, bundle)
		assertModelTokensSum(t, name, metadata)
		if len(metadata.ModelTokens) > 0 {
			withTokens++
		}
	}
	if withTokens < 4 {
		t.Errorf("only %d fixtures carry token accounting, so the invariant is barely exercised", withTokens)
	}
}

// Hostile numbers: a negative, fractional, oversized, or non-numeric count is
// ignored; a valid one at the bound saturates. Nothing in Counts or
// ModelTokens leaves 0..2^53, and the per-model split still adds up whenever
// nothing saturated.
func TestModelTokensIgnoreAndBoundHostileCounts(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","message":{"id":"m1","role":"assistant","model":"claude-opus-synthetic","usage":{"input_tokens":-5,"output_tokens":1.5,"cache_read_input_tokens":9007199254740994,"cache_creation_input_tokens":"12","output_tokens_details":{"thinking_tokens":-1}},"content":[{"type":"text","text":"a"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-01T00:00:02Z","message":{"id":"m2","role":"assistant","model":"claude-opus-synthetic","usage":{"input_tokens":4,"output_tokens_details":{"thinking_tokens":"3"}},"content":[{"type":"text","text":"b"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-01T00:00:03Z","message":{"id":"m3","role":"assistant","model":"claude-sonnet-synthetic","usage":{"input_tokens":6,"output_tokens_details":{"thinking_tokens":2}},"content":[{"type":"text","text":"c"}]}}`,
		`{"type":"assistant","timestamp":"2026-01-01T00:00:04Z","message":{"id":"m4","role":"assistant","model":"claude-haiku-synthetic","usage":{"input_tokens":-1,"output_tokens":0.25},"content":[{"type":"text","text":"d"}]}}`,
	}
	metadata := parserTestMetadata(t, claudeLines(t, lines...))
	countIs(t, "input tokens", metadata.Counts.InputTokens, 10)
	countIs(t, "reasoning tokens", metadata.Counts.ReasoningTokens, 2)
	if metadata.Counts.OutputTokens != nil || metadata.Counts.CacheReadTokens != nil || metadata.Counts.CacheWriteTokens != nil {
		t.Fatalf("hostile counts were read: %#v", metadata.Counts)
	}
	want := []ModelTokens{
		{Model: "claude-opus-synthetic", InputTokens: intp(4)},
		{Model: "claude-sonnet-synthetic", InputTokens: intp(6), ReasoningTokens: intp(2)},
	}
	if !reflect.DeepEqual(metadata.ModelTokens, want) {
		t.Fatalf("model tokens = %s\nwant %s", asJSON(metadata.ModelTokens), asJSON(want))
	}
	// The model whose only usage was unreadable has no entry, not an empty one.
	assertModelTokensSum(t, "hostile", metadata)
}

func TestModelTokensSaturateAtTheBound(t *testing.T) {
	t.Parallel()
	var totals tokenTotals
	for i := range 1100 {
		totals.observe(map[string]any{
			"input_tokens": float64(maxTokenCount), "output_tokens_details": map[string]any{"thinking_tokens": float64(maxTokenCount)},
		}, fmt.Sprintf("m%d", i), []string{"a", "b", ""}[i%3])
	}
	total, split := totals.usage()
	if total.Input == nil || *total.Input != maxTokenCount || total.Reasoning == nil || *total.Reasoning != maxTokenCount {
		t.Fatalf("totals = %#v", total)
	}
	if len(split) != 3 {
		t.Fatalf("split = %s", asJSON(split))
	}
	for _, entry := range split {
		if entry.InputTokens == nil || *entry.InputTokens != maxTokenCount || entry.ReasoningTokens == nil || *entry.ReasoningTokens != maxTokenCount {
			t.Errorf("%s did not saturate at the bound: %s", entry.Model, asJSON(entry))
		}
	}
}

// Streamed duplicates dedupe per model: the same message id repeated counts
// once for its model, and a distinct message of the same model adds.
func TestModelTokensDedupeStreamedRecordsPerModel(t *testing.T) {
	t.Parallel()
	var totals tokenTotals
	usage := func(input float64) map[string]any { return map[string]any{"input_tokens": input} }
	totals.observe(usage(10), "m1", "a")
	totals.observe(usage(10), "m1", "a")
	totals.observe(usage(10), "m1", "a")
	totals.observe(usage(7), "m2", "b")
	totals.observe(usage(7), "m2", "b")
	totals.observe(usage(1), "m3", "a")
	total, split := totals.usage()
	want := []ModelTokens{{Model: "a", InputTokens: intp(11)}, {Model: "b", InputTokens: intp(7)}}
	if !reflect.DeepEqual(split, want) || total.Input == nil || *total.Input != 18 {
		t.Fatalf("total %v split %s", total.Input, asJSON(split))
	}
}

func TestReasoningTokenCountReadsBothHarnessSpellings(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		source map[string]any
		want   int
		ok     bool
	}{
		"codex":            {map[string]any{"reasoning_output_tokens": 12.0}, 12, true},
		"claude":           {map[string]any{"output_tokens_details": map[string]any{"thinking_tokens": 7.0}}, 7, true},
		"openai style":     {map[string]any{"output_tokens_details": map[string]any{"reasoning_tokens": 3.0}}, 3, true},
		"explicit zero":    {map[string]any{"reasoning_output_tokens": 0.0}, 0, true},
		"absent":           {map[string]any{"output_tokens": 5.0}, 0, false},
		"details not map":  {map[string]any{"output_tokens_details": 5.0}, 0, false},
		"negative":         {map[string]any{"reasoning_output_tokens": -1.0}, 0, false},
		"fractional":       {map[string]any{"reasoning_output_tokens": 0.5}, 0, false},
		"non-number":       {map[string]any{"reasoning_output_tokens": "9"}, 0, false},
		"top level wins":   {map[string]any{"reasoning_output_tokens": 1.0, "output_tokens_details": map[string]any{"thinking_tokens": 9.0}}, 1, true},
		"fallback on junk": {map[string]any{"reasoning_output_tokens": -1.0, "output_tokens_details": map[string]any{"thinking_tokens": 9.0}}, 9, true},
	} {
		got, ok := reasoningTokenCount(tc.source)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMCPServer(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"mcp__github__create_issue":                "github",
		"mcp__Claude_Code_iOS_Simulator__control":  "Claude_Code_iOS_Simulator",
		"mcp__a-b.c__tool__with__underscores":      "a-b.c",
		"mcp__1a59c906-04da-521d-bda7-7f7__update": "1a59c906-04da-521d-bda7-7f7",
		"mcp__server":     "",
		"mcp__server__":   "",
		"mcp____tool":     "",
		"mcp__":           "",
		"Bash":            "",
		"MCP__github__x":  "",
		"xmcp__github__x": "",
		"search_docs":     "",
		"":                "",
	} {
		got, ok := mcpServer(name)
		if got != want || ok != (want != "") {
			t.Errorf("mcpServer(%q) = (%q, %v), want %q", name, got, ok, want)
		}
	}
}

// mcp_calls is ordered like tools_used and holds at most MaxMCPCalls servers.
func TestDeriveMCPCallsSortsAndCaps(t *testing.T) {
	t.Parallel()
	counts := map[string]int{"mcp__zeta__a": 3, "mcp__alpha__b": 3, "mcp__alpha__c": 1, "mcp__mid__x": 2, "Bash": 9}
	for i := range MaxMCPCalls + 5 {
		counts[fmt.Sprintf("mcp__s%03d__t", i)] = 1
	}
	got := deriveMCPCalls(namedCalls(counts), "")
	if len(got) != MaxMCPCalls {
		t.Fatalf("len = %d, want the cap %d", len(got), MaxMCPCalls)
	}
	if want := []ToolUsage{{"alpha", 4}, {"zeta", 3}, {"mid", 2}, {"s000", 1}}; !reflect.DeepEqual(got[:4], want) {
		t.Fatalf("mcp calls = %v, want prefix %v", got[:4], want)
	}
	if got := deriveMCPCalls(namedCalls(map[string]int{"Bash": 2, "search_docs": 1}), ""); got != nil {
		t.Fatalf("no MCP calls = %v, want nil", got)
	}
}

// A server name is redacted and length-bounded like a tool name.
func TestDeriveMCPCallsBoundsServerNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("s", 300)
	got := deriveMCPCalls(namedCalls(map[string]int{"mcp__" + long + "__tool": 1}), "")
	if len(got) != 1 || len([]rune(got[0].Name)) != toolNameLimit {
		t.Fatalf("mcp calls = %v", got)
	}
}

// tool_errors counts the results flagged is_error where the harness writes the
// flag, and is unknown, not zero, where it does not.
func TestToolErrorsObservable(t *testing.T) {
	t.Parallel()
	for harness, want := range map[string]bool{"claude": true, "claude-code": true, "cursor": true, "codex": false, "other": false} {
		bundle := SourceBundle{Capture: SourceCapture{Harness: Harness{Name: harness}}}
		if got := toolErrorsObservable(bundle); got != want {
			t.Errorf("toolErrorsObservable(%s) = %v, want %v", harness, got, want)
		}
	}
}

// Metadata written by parser 0.14.0 carries the new fields under the JSON
// names the schema documents, and omits them when unknown.
func TestNewMetadataFieldsJSONNames(t *testing.T) {
	t.Parallel()
	_, metadata := parsedFixture(t, "claude", "claude-model-tokens.jsonl")
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	counts := doc["counts"].(map[string]any)
	for _, key := range []string{"reasoning_tokens", "tool_errors"} {
		if _, ok := counts[key]; !ok {
			t.Errorf("counts has no %s: %v", key, counts)
		}
	}
	for _, key := range []string{"model_tokens", "mcp_calls"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("metadata has no %s", key)
		}
	}
	if metadata.Parser.Version != "0.14.0" {
		t.Errorf("parser version = %q, want 0.14.0", metadata.Parser.Version)
	}
	_, empty := parsedFixture(t, "cursor", "cursor-turn.jsonl")
	raw, _ = json.Marshal(empty)
	for _, key := range []string{"model_tokens", "mcp_calls", "reasoning_tokens"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("an unknown %s was published: %s", key, raw)
		}
	}
}

func asJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
