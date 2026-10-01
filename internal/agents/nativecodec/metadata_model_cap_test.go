package nativecodec

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

// Models with equal tokens are kept by name, lowest first, whatever order the
// map yields them in.
func TestModelTokensFoldKeepsTiedModelsByName(t *testing.T) {
	t.Parallel()
	var totals tokenTotals
	const models = MaxModelTokens + 8
	for i := range models {
		totals.observe(map[string]any{"input_tokens": 5.0}, "m"+strconv.Itoa(i), fmt.Sprintf("t%02d", models-1-i))
	}
	total, split := totals.usage()
	assertModelTokensSum(t, "ties", Metadata{Counts: Counts{InputTokens: total.Input}, ModelTokens: split})
	want := map[string]bool{OtherModels: true}
	for i := range MaxModelTokens - 1 {
		want[fmt.Sprintf("t%02d", i)] = true
	}
	for _, entry := range split {
		if !want[entry.Model] {
			t.Errorf("kept %q, want the %d lowest names", entry.Model, MaxModelTokens-1)
		}
		delete(want, entry.Model)
	}
	if len(want) > 0 {
		t.Errorf("missing %v", want)
	}
}

// What counts toward a model staying is every token it used except
// reasoning, which is already in its output. A field none of the folded models
// reported stays unknown on the entry they are added into, and one any of them
// reported is carried over.
func TestModelTokensFoldWeighsEveryFieldAndKeepsUnknownFieldsNil(t *testing.T) {
	t.Parallel()
	var totals tokenTotals
	add := func(model string, usage map[string]any) {
		totals.observe(usage, "msg-"+model, model)
	}
	for i := range MaxModelTokens - 1 {
		add(fmt.Sprintf("filler-%02d", i), map[string]any{"input_tokens": 100.0})
	}
	add("cache-read", map[string]any{"cache_read_input_tokens": 1000.0})
	add("output", map[string]any{"output_tokens": 1000.0})
	add("cache-write", map[string]any{"cache_creation_input_tokens": 1000.0})
	add("reasoning", map[string]any{"output_tokens": 1.0, "output_tokens_details": map[string]any{"thinking_tokens": 1000.0}})
	add("tiny-write", map[string]any{"cache_creation_input_tokens": 1.0})
	total, split := totals.usage()
	assertModelTokensSum(t, "weights", Metadata{Counts: Counts{
		InputTokens: total.Input, OutputTokens: total.Output, CacheReadTokens: total.CacheRead,
		CacheWriteTokens: total.CacheWrite, ReasoningTokens: total.Reasoning,
	}, ModelTokens: split})
	kept := map[string]ModelTokens{}
	for _, entry := range split {
		kept[entry.Model] = entry
	}
	for _, name := range []string{"cache-read", "output", "cache-write"} {
		if _, ok := kept[name]; !ok {
			t.Errorf("%s has 1000 tokens and was folded away", name)
		}
	}
	if _, ok := kept["reasoning"]; ok {
		t.Error("reasoning tokens counted twice toward staying: the model has one output token")
	}
	other := kept[OtherModels]
	// Folded: the last three fillers by name, "reasoning", and "tiny-write".
	if other.InputTokens == nil || *other.InputTokens != 300 ||
		other.OutputTokens == nil || *other.OutputTokens != 1 ||
		other.ReasoningTokens == nil || *other.ReasoningTokens != 1000 ||
		other.CacheWriteTokens == nil || *other.CacheWriteTokens != 1 {
		t.Errorf("other = %s", asJSON(other))
	}
	if other.CacheReadTokens != nil {
		t.Errorf("no folded model reported cache reads, but other has %d", *other.CacheReadTokens)
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
	var edge tokenTotals
	edge.observe(map[string]any{"input_tokens": 1.0}, "", short)
	_, edgeSplit := edge.usage()
	if got := edgeSplit[0].Model; got != short {
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

// The many-models fixtures name 43 distinct models (40 numbered ones, a real
// "other", a real "unknown", and two 130-character names that differ only
// past the cut) on one harness each, Codex's through per-turn switching.
// Whatever the harness, the split stays within the cap, has no repeated name,
// keeps a real "other" as the one entry the overflow is added to, and sums to
// the session's counts.
func TestManyModelsFixturesAreBoundedAndSum(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct {
		harness string
		file    string
	}{
		{"claude", "claude-many-models.jsonl"}, {"codex", "codex-many-models.jsonl"},
	} {
		_, metadata := parsedFixture(t, fixture.harness, fixture.file)
		label := fixture.file
		assertModelTokensSum(t, label, metadata)
		if len(metadata.ModelTokens) != MaxModelTokens-1 {
			t.Errorf("%s: %d entries, want the %d models with the most tokens (the real %q among them, taking the overflow): %s",
				label, len(metadata.ModelTokens), MaxModelTokens-1, OtherModels, asJSON(metadata.ModelTokens))
		}
		byName := map[string]ModelTokens{}
		for _, entry := range metadata.ModelTokens {
			byName[entry.Model] = entry
			if n := utf8.RuneCountInString(entry.Model); n > maxModelNameRunes {
				t.Errorf("%s: model of %d runes", label, n)
			}
		}
		for _, name := range []string{OtherModels, UnknownModel, "claude-model-039"} {
			if _, ok := byName[name]; !ok {
				t.Errorf("%s: %q is missing", label, name)
			}
		}
		if _, ok := byName["claude-model-000"]; ok {
			t.Errorf("%s: the smallest model was kept", label)
		}
		cut := strings.Repeat("é", maxModelNameRunes-1) + "…"
		if _, ok := byName[cut]; !ok {
			t.Errorf("%s: the two long names were not merged under their cut form", label)
		}
	}
}
