package archive

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// A hostile transcript could name a new model on every record. The split stays
// at MaxModelTokens entries, keeps the models with the most tokens, adds the
// rest together under OtherModels, and so still sums to the session's counts.
func TestModelTokensAreBoundedAndStillSum(t *testing.T) {
	t.Parallel()
	var totals tokenTotals
	const models = 3 * MaxModelTokens
	for i := range models {
		// Model i has i+1 input tokens (1 output), so the largest are the
		// highest numbered; the names sort against the sizes.
		totals.observe(map[string]any{
			"input_tokens": float64(i + 1), "output_tokens": 1.0,
			"output_tokens_details": map[string]any{"thinking_tokens": 1.0},
		}, fmt.Sprintf("msg-%d", i), fmt.Sprintf("model-%03d", i))
	}
	total, split := totals.usage()
	if len(split) != MaxModelTokens {
		t.Fatalf("%d entries, want the cap %d", len(split), MaxModelTokens)
	}
	kept := map[string]ModelTokens{}
	for _, entry := range split {
		kept[entry.Model] = entry
	}
	for i := models - (MaxModelTokens - 1); i < models; i++ {
		if _, ok := kept[fmt.Sprintf("model-%03d", i)]; !ok {
			t.Errorf("model-%03d has tokens among the most and was folded away", i)
		}
	}
	other, ok := kept[OtherModels]
	if !ok {
		t.Fatalf("no %q entry in %s", OtherModels, asJSON(split))
	}
	// The folded models are the smallest 2*32+1 of 96, each with 1 output and
	// 1 reasoning token.
	folded := models - (MaxModelTokens - 1)
	if other.OutputTokens == nil || *other.OutputTokens != folded || other.ReasoningTokens == nil || *other.ReasoningTokens != folded {
		t.Errorf("other = %s, want %d output and reasoning tokens", asJSON(other), folded)
	}
	metadata := Metadata{Counts: Counts{
		InputTokens: total.Input, OutputTokens: total.Output, CacheReadTokens: total.CacheRead,
		CacheWriteTokens: total.CacheWrite, ReasoningTokens: total.Reasoning,
	}, ModelTokens: split}
	assertModelTokensSum(t, "folded", metadata)
	// Which models stay does not depend on map order.
	for range 5 {
		if _, again := totals.usage(); !reflect.DeepEqual(again, split) {
			t.Fatalf("a second pass split differently:\n%s\n%s", asJSON(again), asJSON(split))
		}
	}
}

// A model already named "other" is one of the folded-into entry, not a
// second entry with the same name.
func TestModelTokensFoldIntoAModelNamedOther(t *testing.T) {
	t.Parallel()
	var totals tokenTotals
	totals.observe(map[string]any{"input_tokens": 1e6}, "big", OtherModels)
	for i := range MaxModelTokens + 3 {
		totals.observe(map[string]any{"input_tokens": float64(i + 1)}, fmt.Sprintf("m%d", i), fmt.Sprintf("model-%03d", i))
	}
	total, split := totals.usage()
	seen := map[string]bool{}
	for _, entry := range split {
		if seen[entry.Model] {
			t.Fatalf("%q twice in %s", entry.Model, asJSON(split))
		}
		seen[entry.Model] = true
	}
	if len(split) > MaxModelTokens {
		t.Fatalf("%d entries", len(split))
	}
	assertModelTokensSum(t, "other", Metadata{Counts: Counts{InputTokens: total.Input}, ModelTokens: split})
}

// One long model name cannot make the sidecar large: it is cut, and two names
// that differ only past the cut are one entry.
func TestModelTokensBoundModelNames(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 500)
	var totals tokenTotals
	totals.observe(map[string]any{"input_tokens": 1.0}, "a", long+"1")
	totals.observe(map[string]any{"input_tokens": 2.0}, "b", long+"2")
	total, split := totals.usage()
	if len(split) != 1 || utf8.RuneCountInString(split[0].Model) != maxModelNameRunes || !strings.HasSuffix(split[0].Model, "…") {
		t.Fatalf("split = %s", asJSON(split))
	}
	assertModelTokensSum(t, "long", Metadata{Counts: Counts{InputTokens: total.Input}, ModelTokens: split})
	short := strings.Repeat("m", maxModelNameRunes)
	if got := boundModelName(short); got != short {
		t.Errorf("a name of exactly %d runes was cut", maxModelNameRunes)
	}
}

// The schema accepts what the parser can write at its limits (MaxModelTokens
// entries, a maxModelNameRunes-rune model, MaxMCPCalls servers), rejects
// anything past them, and still accepts a sidecar with none of the 0.14.0
// fields, as every older sidecar is.
func TestMetadataSchemaBoundsModelTokensAndMCPCalls(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, "metadata.schema.json")
	base := parserTestMetadata(t, claudeLines(t,
		`{"type":"user","timestamp":"2026-09-24T10:00:00Z","message":{"role":"user","content":"go"}}`,
	))
	validate := func(m Metadata) error {
		t.Helper()
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var instance any
		if err := json.Unmarshal(data, &instance); err != nil {
			t.Fatal(err)
		}
		return schema.Validate(instance)
	}
	if err := validate(base); err != nil {
		t.Fatalf("a sidecar without the 0.14.0 fields is invalid: %v", err)
	}
	var totals tokenTotals
	for i := range MaxModelTokens + 10 {
		totals.observe(map[string]any{"input_tokens": float64(i + 1)}, strconv.Itoa(i), strings.Repeat("é", 200)+strconv.Itoa(i))
		totals.observe(map[string]any{"input_tokens": 1.0}, "x"+strconv.Itoa(i), fmt.Sprintf("model-%03d", i))
	}
	base.Counts.InputTokens, base.ModelTokens = nil, nil
	_, base.ModelTokens = totals.usage()
	counts := map[string]int{}
	for i := range MaxMCPCalls + 5 {
		counts[fmt.Sprintf("mcp__server-%02d__tool", i)] = 1
	}
	base.MCPCalls = deriveMCPCalls(namedCalls(counts), "")
	if len(base.ModelTokens) != MaxModelTokens || len(base.MCPCalls) != MaxMCPCalls {
		t.Fatalf("%d model entries, %d MCP servers", len(base.ModelTokens), len(base.MCPCalls))
	}
	if err := validate(base); err != nil {
		t.Fatalf("metadata at the limits is invalid: %v", err)
	}
	for name, mutate := range map[string]func(*Metadata){
		"too many models":  func(m *Metadata) { m.ModelTokens = append(m.ModelTokens, ModelTokens{Model: "extra"}) },
		"long model":       func(m *Metadata) { m.ModelTokens[0].Model = strings.Repeat("x", maxModelNameRunes+1) },
		"empty model":      func(m *Metadata) { m.ModelTokens[0].Model = "" },
		"negative tokens":  func(m *Metadata) { n := -1; m.ModelTokens[0].InputTokens = &n },
		"negative reason":  func(m *Metadata) { n := -1; m.Counts.ReasoningTokens = &n },
		"negative errors":  func(m *Metadata) { n := -1; m.Counts.ToolErrors = &n },
		"too many servers": func(m *Metadata) { m.MCPCalls = append(m.MCPCalls, ToolUsage{"extra", 1}) },
		"zero MCP count":   func(m *Metadata) { m.MCPCalls[0].Count = 0 },
	} {
		m := base
		m.ModelTokens = append([]ModelTokens(nil), base.ModelTokens...)
		m.MCPCalls = append([]ToolUsage(nil), base.MCPCalls...)
		mutate(&m)
		if validate(m) == nil {
			t.Errorf("%s: schema accepted it", name)
		}
	}
}
