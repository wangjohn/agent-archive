package stats

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestDefaultPriceTableIsValid(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	if table.Version == "" || table.Currency != "USD" || len(table.Sources) == 0 || table.Notes == "" {
		t.Fatalf("table header = %+v", table)
	}
	asOf, err := time.Parse("2006-01-02", table.AsOf)
	if err != nil {
		t.Fatalf("as_of = %q", table.AsOf)
	}
	if asOf.After(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) || asOf.Before(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("as_of %s is not a plausible date", table.AsOf)
	}
	seen := map[string]bool{}
	for _, m := range table.Models {
		if seen[m.ID] || m.ID != NormalizeModel(m.ID) {
			t.Errorf("model id %q repeated or not normalized", m.ID)
		}
		seen[m.ID] = true
		if m.Family == "" || m.InputPerMTok <= 0 || m.OutputPerMTok <= 0 || m.CacheReadPerMTok <= 0 {
			t.Errorf("%s has a missing price: %+v", m.ID, m)
		}
		if m.CacheReadPerMTok > m.InputPerMTok || m.OutputPerMTok < m.InputPerMTok {
			t.Errorf("%s prices are not ordered as list prices are: %+v", m.ID, m)
		}
		if strings.HasPrefix(m.ID, "claude-") && m.CacheWritePerMTok < m.InputPerMTok {
			t.Errorf("%s: a cache write costs at least the input price: %+v", m.ID, m)
		}
	}
	for _, source := range table.Sources {
		if !strings.HasPrefix(source, "https://") {
			t.Errorf("source %q is not a URL", source)
		}
	}
}

func TestRealModelIDsThePriceTableKnows(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	for _, tc := range []struct {
		id     string
		priced bool
		family string
	}{
		// Model ids seen in real Claude Code and Codex archives.
		{"claude-opus-5-5", true, "opus"},
		{"claude-opus-5-5[1m]", true, "opus"},
		{"claude-opus-5", true, "opus"},
		{"claude-opus-4-8", true, "opus"},
		{"claude-fable-5-1", true, "fable"},
		{"claude-sonnet-5-5", true, "sonnet"},
		{"claude-sonnet-5", true, "sonnet"},
		{"claude-haiku-4-5-20251001", true, "haiku"},
		{"Claude-Haiku-4-5@20251001", true, "haiku"},
		{"claude-opus-4-20250514", true, "opus"},
		{"gpt-5.6-terra", true, "gpt-5.6"},
		{"gpt-6-luna", true, "gpt-6"},
		{"gpt-5", true, "gpt-5"},
		// Not priced, never guessed.
		{"codex-auto-review", false, ""},
		{"unknown", false, ""},
		{"opus", false, ""},
		{"fable", false, ""},
		{"<synthetic>", false, ""},
		{"jev-latest", false, ""},
		{"claude-opus-5-6", false, ""}, // a later version is not the earlier one
		{"claude-opus-5-5-fast", false, ""},
		{"", false, ""},
	} {
		entry, ok := table.Lookup(tc.id)
		if ok != tc.priced || (ok && entry.Family != tc.family) {
			t.Errorf("Lookup(%q) = %+v, %v; want priced=%v family=%q", tc.id, entry, ok, tc.priced, tc.family)
		}
	}
}

func TestNormalizeModel(t *testing.T) {
	t.Parallel()
	if got := NormalizeModel("  claude-opus-5-5  "); got != "claude-opus-5-5" {
		t.Fatalf("NormalizeModel did not trim: %q", got)
	}
	for in, want := range map[string]string{
		"Claude-Opus-5-5[1m]":          "claude-opus-5-5",
		"claude-haiku-4-5-20251001":    "claude-haiku-4-5",
		"claude-haiku-4-5@20251001":    "claude-haiku-4-5",
		"claude-opus-4-1":              "claude-opus-4-1", // a version, not a date
		"claude-3-5-sonnet-2024102":    "claude-3-5-sonnet-2024102",
		"gpt-5.6-terra":                "gpt-5.6-terra",
		"":                             "",
		"claude-sonnet-4-20250514[1m]": "claude-sonnet-4",
	} {
		if got := NormalizeModel(in); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
}

const validTable = `{"version":"custom-1","as_of":"2026-09-01","models":[
 {"id":"claude-opus-5-5","family":"opus","input_per_mtok":1,"output_per_mtok":2,"cache_read_per_mtok":0.1,"cache_write_per_mtok":1.25},
 {"id":"my-model","input_per_mtok":3,"output_per_mtok":4,"cache_read_per_mtok":0,"cache_write_per_mtok":0}]}`

func TestParsePriceTable(t *testing.T) {
	t.Parallel()
	got, err := ParsePriceTable([]byte(validTable))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "custom-1" || got.Currency != "USD" || len(got.Models) != 2 || got.Models[1].Family != "my-model" || got.Models[1].CacheReadPerMTok != 0 {
		t.Fatalf("table = %+v", got)
	}
	for name, doc := range map[string]string{
		"not json":         `{`,
		"no version":       `{"as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"bad date":         `{"version":"v","as_of":"Sept 2026","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"no models":        `{"version":"v","as_of":"2026-09-01","models":[]}`,
		"missing price":    `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1}]}`,
		"negative":         `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":-1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"unknown field":    `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1,"cache_per_mtok":1}]}`,
		"duplicate id":     `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1},{"id":"A[1m]","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"empty id":         `{"version":"v","as_of":"2026-09-01","models":[{"id":" ","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"trailing garbage": validTable + `{}`,
	} {
		if _, err := ParsePriceTable([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParsePriceTable([]byte(strings.Replace(validTable, `"cache_read_per_mtok":0.1,`, "", 1))); err == nil || !strings.Contains(err.Error(), "cache_read_per_mtok") {
		t.Errorf("a missing price must name the field, got %v", err)
	}
}

func TestOverridesReplaceAndAddEntries(t *testing.T) {
	t.Parallel()
	custom, err := ParsePriceTable([]byte(validTable))
	if err != nil {
		t.Fatal(err)
	}
	merged := DefaultPriceTable().WithOverrides(custom)
	if !merged.Overridden || merged.Version != "custom-1" || merged.AsOf != "2026-09-01" {
		t.Fatalf("merged header = %+v", merged)
	}
	if e, _ := merged.Lookup("claude-opus-5-5"); e.InputPerMTok != 1 || e.OutputPerMTok != 2 {
		t.Fatalf("the override did not replace: %+v", e)
	}
	if _, ok := merged.Lookup("my-model"); !ok {
		t.Fatal("the new model was not added")
	}
	if e, ok := merged.Lookup("claude-sonnet-5-5"); !ok || e.InputPerMTok != 2 {
		t.Fatalf("an untouched default was lost: %+v", e)
	}
	if len(merged.Models) != len(DefaultPriceTable().Models)+1 {
		t.Fatalf("merged has %d models", len(merged.Models))
	}
	if DefaultPriceTable().Overridden {
		t.Fatal("the default table is marked overridden")
	}

	m := meta("a", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 1_000_000, 0, 0, 0))
	got := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: merged})
	if v := got.Overview.Cost.Value; v == nil || !near(*v, 1) || got.Prices.Version != "custom-1" || !got.Prices.Overridden {
		t.Fatalf("cost %v prices %+v", f64(v), got.Prices)
	}
}

func TestAnExplicitEmptyTableLeavesEverythingUnpriced(t *testing.T) {
	t.Parallel()
	m := meta("a", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 1000, 0, 0, 0))
	got := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: PriceTable{Version: "none", AsOf: "2026-09-01"}})
	if got.Overview.Cost.Value != nil || !got.Overview.Cost.Partial || got.Prices.Version != "none" {
		t.Fatalf("cost = %+v prices %+v", got.Overview.Cost, got.Prices)
	}
	dflt := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork})
	if dflt.Prices.Version != DefaultPriceTable().Version || dflt.Prices.AsOf != DefaultPriceTable().AsOf {
		t.Fatalf("default prices = %+v", dflt.Prices)
	}
}

func TestEachTokenTypeIsPricedSeparately(t *testing.T) {
	t.Parallel()
	table := PriceTable{Version: "t", AsOf: "2026-09-01", Currency: "USD", Models: []ModelPrice{
		{ID: "m", Family: "m", InputPerMTok: 1, OutputPerMTok: 10, CacheReadPerMTok: 100, CacheWritePerMTok: 1000},
	}}
	for _, tc := range []struct {
		name  string
		input int
		out   int
		read  int
		write int
		want  float64
	}{
		{"fresh input only", 1_000_000, 0, 0, 0, 1},
		{"output only", 0, 1_000_000, 0, 0, 10},
		{"cache read only", 0, 0, 1_000_000, 0, 100},
		{"cache write only", 0, 0, 0, 1_000_000, 1000},
	} {
		m := meta("a", "claude", day(time.September, 28, 10), modelTokens("m", tc.input, tc.out, tc.read, tc.write))
		got := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: table})
		if v := got.Overview.Cost.Value; v == nil || !near(*v, tc.want) {
			t.Errorf("%s: cost = %v, want %v", tc.name, f64(v), tc.want)
		}
	}
	// Reasoning tokens are part of output and add nothing.
	m := meta("a", "claude", day(time.September, 28, 10), modelTokens("m", 0, 1_000_000, 0, 0))
	m.ModelTokens[0].ReasoningTokens = ip(900_000)
	if v := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: table}).Overview.Cost.Value; !near(*v, 10) {
		t.Fatalf("reasoning was priced on top of output: %v", *v)
	}
}
