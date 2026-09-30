package stats

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Real and hostile ids: what each becomes, and whether the built-in table
// prices it. A wrong price is worse than none, so every id that is not the
// same model as a priced one must stay unpriced.
func TestNormalizeModelRealAndHostileIDs(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	for _, tc := range []struct {
		id     string
		want   string
		priced bool
	}{
		// The same model, named another way.
		{"anthropic/claude-opus-5-5", "claude-opus-5-5", true},
		{"us.anthropic.claude-sonnet-5-5-v1:0", "claude-sonnet-5-5", true},
		{"global.anthropic.claude-sonnet-5-5-v1:0", "claude-sonnet-5-5", true},
		{"eu.anthropic.claude-opus-5-5-20250101-v1:0", "claude-opus-5-5", true},
		{"claude-opus-5-5@20250101", "claude-opus-5-5", true},
		{"CLAUDE-OPUS-5-5", "claude-opus-5-5", true},
		{" \tclaude-opus-5-5 \n", "claude-opus-5-5", true},
		{"claude-opus-5-5-20250101[1m]", "claude-opus-5-5", true},
		{"gpt-5-2025-08-07", "gpt-5", true},
		{"openrouter/anthropic/claude-opus-5-5", "claude-opus-5-5", true},
		{"CLAUDE-HAİKU-4-5", "claude-haiku-4-5", true}, // U+0130 lower-cases to i
		// Bedrock's one colon-less version (Opus 4.6), behind its vendor prefix.
		{"us.anthropic.claude-opus-4-6-v1", "claude-opus-4-6", true},
		{"anthropic.claude-opus-4-6-v1[1m]", "claude-opus-4-6", true},
		{"bedrock/us.anthropic.claude-opus-4-6-v1", "claude-opus-4-6", true},
		{"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-opus-4-6-v1", "claude-opus-4-6", true},
		{"anthropic.claude-opus-5-5-v1:0", "claude-opus-5-5", true},
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", "claude-sonnet-4-5", true},
		// Not the same model, or not a model the table names: unpriced.
		{"opus", "opus", false},
		{"deepseek-v3", "deepseek-v3", false},
		{"claude-opus-5-5-v1", "claude-opus-5-5-v1", false}, // no colon: not Bedrock's
		{"anthropic/claude-opus-5-5-v1", "claude-opus-5-5-v1", false},
		{"claude-opus-4-6-20260205-v1", "claude-opus-4-6-20260205-v1", false},
		{"anthropic.claude-opus-4-6-v1-v1", "claude-opus-4-6-v1", false}, // only one layer comes off
		{"anthropic.claude-opus-4-6-v2", "claude-opus-4-6-v2", false},
		{"anthropic.claude-opus-4-6-v1:x", "claude-opus-4-6-v1:x", false},
		{"anthropic.claude-opus-4-6-v\uff11", "claude-opus-4-6-v\uff11", false}, // a full-width 1
		{"anthropic.claude-opus-4-6-v1\u200b", "claude-opus-4-6-v1\u200b", false},
		{"anthropic.gpt-5-v1", "gpt-5-v1", false},                                 // the colon-less version is Claude's
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", "claude-3-5-sonnet", false}, // retired, not listed
		{"o3", "o3", false},
		{"gpt-5-v1:0", "gpt-5-v1:0", false}, // Bedrock's version belongs to Claude ids
		{"gpt-5-pro", "gpt-5-pro", false},
		{"gpt-5-mini-high", "gpt-5-mini-high", false},
		{"ft:gpt-5-mini:acme::abc123", "ft:gpt-5-mini:acme::abc123", false},
		{"claude-opus-5-5-thinking", "claude-opus-5-5-thinking", false},
		{"claude-opus-5-5\u200b", "claude-opus-5-5\u200b", false}, // a zero-width space is another id
		{"\uff43laude-opus-5-5", "\uff43laude-opus-5-5", false},   // a full-width c is another id
		{"foo.bar.anthropic.claude-opus-5-5", "foo.bar.anthropic.claude-opus-5-5", false},
		{"claude-opus-5-5…", "claude-opus-5-5…", false}, // a name cut at 128 runes ends in an ellipsis
		{strings.Repeat("x", 127) + "…", strings.Repeat("x", 127) + "…", false},
		// A run of digits that is not a calendar date is not a date suffix.
		{"claude-opus-4-12345678", "claude-opus-4-12345678", false},
		{"claude-opus-4-1234-56-78", "claude-opus-4-1234-56-78", false},
		{"claude-opus-4-20251301", "claude-opus-4-20251301", false},
		{"claude-opus-4-20250132", "claude-opus-4-20250132", false},
		{"claude-opus-4-20250514", "claude-opus-4", true},
		// Nothing left is nothing.
		{"", "", false}, {"/", "", false}, {"anthropic.", "", false}, {"claude-opus-5-5/", "", false},
		// Layers that come off together, in any order.
		{"claude-opus-5-5-v1:0-20250101", "claude-opus-5-5", true},
		{"claude-opus-5-5[1m]-20250101", "claude-opus-5-5", true},
		{"claude-opus-5-5-20250101-20250101", "claude-opus-5-5", true},
	} {
		if got := NormalizeModel(tc.id); got != tc.want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", tc.id, got, tc.want)
		}
		if _, ok := table.Lookup(tc.id); ok != tc.priced {
			t.Errorf("Lookup(%q) priced = %v, want %v", tc.id, ok, tc.priced)
		}
	}
}

// Normalizing twice is normalizing once, for any pile of prefixes and
// suffixes: the price table stores ids already normalized and looks them up
// again, so an id that changed on the second pass would collide with, and
// replace, another model's price after the duplicate check had passed.
func TestNormalizeModelIsIdempotent(t *testing.T) {
	t.Parallel()
	parts := []string{"claude-opus-5-5", "gpt-5", "us.anthropic.", "anthropic/", "/", "[1m]", "[x]", "-20250101", "@20250101", "-2025-08-07", "-v1:0", "-v1", "anthropic.", "global.anthropic.", "-v2", " ", " ", ".", "-", "x"}
	rng := rand.New(rand.NewPCG(3, 7))
	for range 20000 {
		var id strings.Builder
		for range 1 + rng.IntN(6) {
			id.WriteString(parts[rng.IntN(len(parts))])
		}
		once := NormalizeModel(id.String())
		if twice := NormalizeModel(once); twice != once {
			t.Fatalf("NormalizeModel(%q) = %q, but again %q", id.String(), once, twice)
		}
	}
}

// No rule that unwraps an id may turn one priced model into another: every id
// in the built-in table is its own normal form, and no other priced id decorates
// it (a prefix or a suffix on one priced id would make it another's).
func TestNoPricedModelNormalizesToAnotherPricedModel(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	priced := map[string]bool{}
	for _, entry := range table.Models {
		priced[entry.ID] = true
	}
	for _, entry := range table.Models {
		if got := NormalizeModel(entry.ID); got != entry.ID {
			t.Errorf("priced id %q normalizes to %q", entry.ID, got)
		}
		// Every decoration a real id can carry leads back to this id, never another.
		wrappers := []string{
			entry.ID + "-20250101", entry.ID + "@20250101", entry.ID + "-2025-08-07", entry.ID + "[1m]",
			"anthropic/" + entry.ID, strings.ToUpper(entry.ID),
		}
		if strings.HasPrefix(entry.ID, "claude-") {
			wrappers = append(wrappers, "us.anthropic."+entry.ID+"-v1:0", "anthropic."+entry.ID+"-v1", "global.anthropic."+entry.ID+"-v1")
		}
		for _, wrapped := range wrappers {
			if got := NormalizeModel(wrapped); got != entry.ID {
				t.Errorf("NormalizeModel(%q) = %q, want %q", wrapped, got, entry.ID)
			}
		}
	}
}

// Two entries whose ids differ only by what normalization removes are one
// model twice, however many layers hide it: the duplicate check must see it,
// or the later one silently replaces the earlier price in the index.
func TestPriceTableDuplicatesHiddenBehindLayersAreRejected(t *testing.T) {
	t.Parallel()
	entry := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}`, id)
	}
	for _, hidden := range []string{
		"CLAUDE-OPUS-5-5", "claude-opus-5-5-20250101", "anthropic/claude-opus-5-5", "us.anthropic.claude-opus-5-5-v1:0",
		"claude-opus-5-5-v1:0-20250101", "claude-opus-5-5[1m]-20250101", "claude-opus-5-5-20250101-20250101",
	} {
		doc := `{"version":"v","as_of":"2026-09-01","models":[` + entry("claude-opus-5-5") + `,` + entry(hidden) + `]}`
		if _, err := ParsePriceTable([]byte(doc)); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Errorf("%q hides a duplicate of claude-opus-5-5: err = %v", hidden, err)
		}
	}
	// Overriding by a decorated id replaces the model, it does not add a second one.
	base := DefaultPriceTable()
	custom, err := ParsePriceTable([]byte(`{"version":"c","as_of":"2026-09-01","models":[` + entry("claude-opus-5-5-v1:0-20250101") + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	merged := base.WithOverrides(custom)
	count := 0
	for _, m := range merged.Models {
		if NormalizeModel(m.ID) == "claude-opus-5-5" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("claude-opus-5-5 appears %d times after an override", count)
	}
	if got, ok := merged.index()["claude-opus-5-5"]; !ok || got.InputPerMTok != 1 {
		t.Fatalf("override not applied: %+v", got)
	}
}

// A hand-built entry whose id normalizes to nothing prices nothing, not every
// id that also normalizes to nothing.
func TestEntryWithAnEmptyNormalizedIDPricesNothing(t *testing.T) {
	t.Parallel()
	table := PriceTable{Version: "h", AsOf: "2026-09-01", Currency: "USD", Models: []ModelPrice{
		{ID: "/", Family: "x", InputPerMTok: 1, OutputPerMTok: 1},
	}}
	if _, ok := table.Lookup("anthropic."); ok {
		t.Error("Lookup priced an id that normalizes to nothing")
	}
	if _, ok := table.index()[""]; ok {
		t.Error("index keys an empty id")
	}
}

// Codex reports cache reads and writes inside input_tokens (OpenAI's
// input_tokens_details.cached_tokens and cache_write_tokens are subsets of it).
// A record that says otherwise (read and write together exceed the input) is
// inconsistent: fresh input clamps at zero and both cache counts are kept, so
// nothing is dropped and no count goes negative.
func TestCodexInconsistentCacheCountsClampFreshAndKeepTheCache(t *testing.T) {
	t.Parallel()
	// read 600 fits in input 1000, read + write 1100 does not.
	m := meta("x1", "codex", day(time.September, 28, 15), modelTokens("gpt-6-luna", 1000, 50, 600, 500))
	c := Compute([]archive.Metadata{m}, opts()).Composition
	if c == nil || c.FreshInput.Tokens != 0 || c.CacheRead.Tokens != 600 || c.CacheWrite.Tokens != 500 || c.Output.Tokens != 50 || c.Total != 1150 {
		t.Fatalf("composition = %+v", c)
	}
	// Exactly consistent: read + write == input leaves no fresh input.
	m = meta("x2", "codex", day(time.September, 28, 15), modelTokens("gpt-6-luna", 1000, 50, 600, 400))
	c = Compute([]archive.Metadata{m}, opts()).Composition
	if c.FreshInput.Tokens != 0 || c.Total != 1050 {
		t.Fatalf("composition = %+v", c)
	}
}
