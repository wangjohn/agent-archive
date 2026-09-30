package stats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// flatPrices prices model "m" (family "m") at one currency unit per million
// tokens of every kind, so a session of N million tokens costs exactly N. Any
// other model is unpriced.
func flatPrices() PriceTable {
	return PriceTable{
		Version:  "flat",
		AsOf:     "2026-09-01",
		Currency: "USD",
		Models: []ModelPrice{
			{ID: "m", Family: "m", InputPerMTok: 1, OutputPerMTok: 1, CacheReadPerMTok: 1, CacheWritePerMTok: 1},
		},
	}
}

func flatOptions() Options {
	return Options{Now: now, Location: newYork, PriceTable: flatPrices()}
}

// million is n million cache-read tokens of the priced model "m": n currency
// units, with a cache-hit rate of 100% so that no heads-up note is raised.
func million(n int) option { return modelTokens("m", 0, 0, n*1_000_000, 0) }

func dayCost(tb testing.TB, s Stats, date string) Cost {
	tb.Helper()
	for _, d := range s.Daily {
		if d.Date == date {
			return d.Cost
		}
	}
	tb.Fatalf("no day %s", date)
	return Cost{}
}

// A day's cost is priced from that day's sessions as the overall cost is:
// unknown when nothing on the day could be priced, flagged when part of it
// could not, and a known zero on a day without sessions.
func TestDailyCostFollowsTheOverallCostRules(t *testing.T) {
	t.Parallel()
	at := func(d int) time.Time { return day(time.September, d, 10) }
	cases := []struct {
		name        string
		sessions    []archive.Metadata
		date        string
		wantUSD     *float64
		wantPartial bool
		wantUnpaid  int64
		wantApprox  bool
	}{
		{"a day without sessions is a known zero", []archive.Metadata{meta("a", "claude", at(20), million(2))}, "2026-09-21", fp(0), false, 0, false},
		{"one priced session", []archive.Metadata{meta("a", "claude", at(20), million(2))}, "2026-09-20", fp(2), false, 0, false},
		{"two sessions on a day add up", []archive.Metadata{meta("a", "claude", at(20), million(2)), meta("b", "claude", at(20), million(3))}, "2026-09-20", fp(5), false, 0, false},
		{"a session without token counts is unknown, not zero", []archive.Metadata{meta("a", "cursor", at(20))}, "2026-09-20", nil, false, 0, false},
		{"only an unpriced model is unknown and flagged", []archive.Metadata{meta("a", "claude", at(20), modelTokens("x", 700, 300, 0, 0))}, "2026-09-20", nil, true, 1000, false},
		{"priced and unpriced models on one day are partial", []archive.Metadata{meta("a", "claude", at(20), million(1), modelTokens("x", 700, 300, 0, 0))}, "2026-09-20", fp(1), true, 1000, false},
		{"a session without tokens beside a priced one costs what the priced one does", []archive.Metadata{meta("a", "cursor", at(20)), meta("b", "claude", at(20), million(4))}, "2026-09-20", fp(4), false, 0, false},
		{"a session priced at its main model is approximate", []archive.Metadata{
			meta("a", "claude", at(20), parserVersion("0.9.0"), usedModel("m", 3), tokens(2_000_000, 0, 0, 0)),
		}, "2026-09-20", fp(2), false, 0, true},
		{"cache and output tokens are priced too", []archive.Metadata{meta("a", "claude", at(20), modelTokens("m", 1_000_000, 1_000_000, 1_000_000, 1_000_000))}, "2026-09-20", fp(4), false, 0, false},
		{"a day outside the local day boundary belongs to its own day", []archive.Metadata{
			meta("late", "claude", time.Date(2026, time.September, 20, 23, 59, 0, 0, newYork), million(1)),
			meta("early", "claude", time.Date(2026, time.September, 21, 0, 1, 0, 0, newYork), million(2)),
		}, "2026-09-21", fp(2), false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dayCost(t, Compute(tc.sessions, flatOptions()), tc.date)
			switch {
			case tc.wantUSD == nil && got.USD != nil:
				t.Fatalf("cost = %v, want unknown", f64(got.USD))
			case tc.wantUSD != nil && (got.USD == nil || !near(*got.USD, *tc.wantUSD)):
				t.Fatalf("cost = %v, want %v", f64(got.USD), f64(tc.wantUSD))
			}
			if got.Partial != tc.wantPartial || got.UnpricedTokens != tc.wantUnpaid || got.Approximate != tc.wantApprox {
				t.Fatalf("flags = partial %v unpriced %d approximate %v, want %v %d %v",
					got.Partial, got.UnpricedTokens, got.Approximate, tc.wantPartial, tc.wantUnpaid, tc.wantApprox)
			}
		})
	}
}

// A subagent's cost lands on the day its parent is placed on, the same as its
// tokens do, whatever time the subagent's own capture says.
func TestDailyCostPlacesSubagentsWhereTheirTokensAre(t *testing.T) {
	t.Parallel()
	sessions := []archive.Metadata{
		meta("parent", "claude", day(time.September, 20, 10), million(1)),
		meta("kid", "claude", day(time.September, 25, 10), parentOf("parent"), million(4)),
	}
	got := Compute(sessions, flatOptions())
	parentDay, kidDay := Day{}, Day{}
	for _, d := range got.Daily {
		if d.Date == "2026-09-20" {
			parentDay = d
		}
		if d.Date == "2026-09-25" {
			kidDay = d
		}
	}
	if parentDay.Cost.USD == nil || !near(*parentDay.Cost.USD, 5) || parentDay.Tokens == nil || *parentDay.Tokens != 5_000_000 {
		t.Errorf("the parent's day: cost %v tokens %s, want the subagent's 4 and 1 together", f64(parentDay.Cost.USD), i64(parentDay.Tokens))
	}
	if kidDay.Date == "" || kidDay.Cost.USD == nil || *kidDay.Cost.USD != 0 || kidDay.Sessions != 0 {
		t.Errorf("the subagent's own capture day: %+v, want an empty day", kidDay)
	}
	if got.PeakSpend == nil || got.PeakSpend.Date != "2026-09-20" || !near(got.PeakSpend.USD, 5) {
		t.Errorf("peak spend = %+v", got.PeakSpend)
	}
}

// A sparse archive: three active days of 30 leave 27 known zeros, and the peak
// is the dearest day, not the busiest or the biggest in tokens.
func TestPeakSpendOnASparseArchive(t *testing.T) {
	t.Parallel()
	sessions := []archive.Metadata{
		meta("a", "claude", day(time.September, 10, 9), million(2)),
		meta("b", "claude", day(time.September, 18, 9), million(9)),
		meta("c", "claude", day(time.September, 18, 15), million(1)),
		meta("d", "claude", day(time.September, 27, 9), million(1)),
	}
	got := Compute(sessions, flatOptions())
	if got.PeakSpend == nil || got.PeakSpend.Date != "2026-09-18" || !near(got.PeakSpend.USD, 10) {
		t.Fatalf("peak spend = %+v, want 2026-09-18 at 10", got.PeakSpend)
	}
	active, zero := 0, 0
	for _, d := range got.Daily {
		switch {
		case d.Sessions > 0:
			active++
		case d.Cost.USD != nil && *d.Cost.USD == 0 && !d.Cost.Partial:
			zero++
		}
	}
	if active != 3 || zero != 27 || len(got.Daily) != 30 {
		t.Fatalf("active %d, known-zero %d of %d days, want 3, 27 of 30", active, zero, len(got.Daily))
	}
}

func TestPeakSpendCostsMoreNotMoreTokens(t *testing.T) {
	t.Parallel()
	table := flatPrices()
	table.Models = append(table.Models, ModelPrice{ID: "dear", Family: "dear", InputPerMTok: 100, OutputPerMTok: 100, CacheReadPerMTok: 100, CacheWritePerMTok: 100})
	sessions := []archive.Metadata{
		meta("many", "claude", day(time.September, 10, 9), million(50)),
		meta("few", "claude", day(time.September, 12, 9), modelTokens("dear", 1_000_000, 0, 0, 0)),
	}
	got := Compute(sessions, Options{Now: now, Location: newYork, PriceTable: table})
	if got.Peak == nil || got.Peak.Date != "2026-09-10" {
		t.Fatalf("token peak = %+v", got.Peak)
	}
	if got.PeakSpend == nil || got.PeakSpend.Date != "2026-09-12" || !near(got.PeakSpend.USD, 100) {
		t.Fatalf("peak spend = %+v, want the dear day", got.PeakSpend)
	}
}

func TestPeakSpendTiesGoToTheEarlierDay(t *testing.T) {
	t.Parallel()
	sessions := []archive.Metadata{
		meta("late", "claude", day(time.September, 20, 9), million(3)),
		meta("early", "claude", day(time.September, 5, 9), million(3)),
	}
	got := Compute(sessions, flatOptions())
	if got.PeakSpend == nil || got.PeakSpend.Date != "2026-09-05" {
		t.Fatalf("peak spend = %+v, want the earlier of two equal days", got.PeakSpend)
	}
}

// No priced cost above zero, no peak: nothing priced, unknown tokens, and
// reported zero tokens all leave it out.
func TestNoPeakSpendWithoutAPricedCost(t *testing.T) {
	t.Parallel()
	cases := map[string][]archive.Metadata{
		"no sessions":       nil,
		"only Cursor":       {meta("a", "cursor", day(time.September, 20, 9))},
		"only unpriced":     {meta("a", "claude", day(time.September, 20, 9), modelTokens("x", 5, 5, 5, 5))},
		"only zero tokens":  {meta("a", "claude", day(time.September, 20, 9), modelTokens("m", 0, 0, 0, 0))},
		"only a prior span": {meta("a", "claude", day(time.July, 20, 9), million(3))},
	}
	for name, sessions := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Compute(sessions, flatOptions())
			if got.PeakSpend != nil {
				t.Fatalf("peak spend = %+v, want none", got.PeakSpend)
			}
			mustJSON(t, got)
		})
	}
}

// The days' costs and the overall cost are one number, however an archive is
// laid out (unpriced models included: their tokens are counted as unpriced
// on the day, and the priced rest adds up): the property that makes a spend
// chart's bars add up to the headline.
func TestDailyCostsAddUpToTheOverallCost(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*104729))
		sessions, _ := randomArchive(rng, 40+rng.IntN(160))
		wide := Options{Now: time.Date(2026, time.October, 1, 0, 0, 0, 0, newYork), Location: newYork, Days: 400}
		got := Compute(sessions, wide)
		var sum float64
		var unpriced int64
		peak := 0.0
		for _, d := range got.Daily {
			if d.Cost.USD != nil {
				sum += *d.Cost.USD
				peak = max(peak, *d.Cost.USD)
			}
			unpriced += d.Cost.UnpricedTokens
			if d.Tokens == nil && d.Cost.USD != nil {
				t.Fatalf("seed %d: day %s has unknown tokens and a known cost %v", seed, d.Date, *d.Cost.USD)
			}
		}
		overall := 0.0
		if got.Overview.Cost.Value != nil {
			overall = *got.Overview.Cost.Value
		}
		if !near2(sum, overall) {
			t.Errorf("seed %d: days' costs add to %v, overall %v", seed, sum, overall)
		}
		if unpriced != got.Overview.Cost.UnpricedTokens {
			t.Errorf("seed %d: days' unpriced tokens %d, overall %d", seed, unpriced, got.Overview.Cost.UnpricedTokens)
		}
		switch {
		case got.PeakSpend == nil && peak > 0:
			t.Errorf("seed %d: no peak spend but a day cost %v", seed, peak)
		case got.PeakSpend != nil && got.PeakSpend.USD != peak:
			t.Errorf("seed %d: peak spend %v, dearest day %v", seed, got.PeakSpend.USD, peak)
		}
	}
}

// The daily costs are the same for the same sessions in any order.
func TestDailyCostsDoNotDependOnInputOrder(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 7))
	sessions, _ := randomArchive(rng, 150)
	o := Options{Now: time.Date(2026, time.September, 20, 12, 0, 0, 0, newYork), Location: newYork, Days: 90}
	want := Compute(sessions, o)
	for range 5 {
		shuffled := append([]archive.Metadata(nil), sessions...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := Compute(shuffled, o)
		if mustJSON(t, got.Daily) != mustJSON(t, want.Daily) || mustJSON(t, got.PeakSpend) != mustJSON(t, want.PeakSpend) {
			t.Fatal("the daily costs or the peak spend changed with the order of the input")
		}
	}
}

func TestCacheShare(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	cases := []struct {
		name     string
		sessions []archive.Metadata
		want     *float64
	}{
		{"nothing", nil, nil},
		{"only Cursor, no token counts", []archive.Metadata{meta("a", "cursor", at)}, nil},
		{"counts without cache fields are not zero hits", []archive.Metadata{
			meta("a", "claude", at, func(m *archive.Metadata) { m.Counts.InputTokens, m.Counts.OutputTokens = ip(100), ip(50) }),
		}, nil},
		{"no cache reads at all is a real zero", []archive.Metadata{meta("a", "claude", at, modelTokens("m", 100, 100, 0, 0))}, fp(0)},
		{"97 in a hundred", []archive.Metadata{meta("a", "claude", at, modelTokens("m", 1, 2, 97, 0))}, fp(0.97)},
		{"all cache reads", []archive.Metadata{meta("a", "claude", at, modelTokens("m", 0, 0, 10, 0))}, fp(1)},
		{"reads over every token, writes and output too", []archive.Metadata{meta("a", "claude", at, modelTokens("m", 10, 20, 30, 40))}, fp(0.3)},
		{"a session without counts beside one with them", []archive.Metadata{
			meta("a", "cursor", at), meta("b", "claude", at, modelTokens("m", 50, 0, 50, 0)),
		}, fp(0.5)},
		{"Codex's cached input is not double counted", []archive.Metadata{meta("a", "codex", at, modelTokens("gpt-5.6-terra", 100, 0, 80, 0))}, fp(0.8)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Compute(tc.sessions, flatOptions()).Overview.CacheShare
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("cache share = %v, want unknown", f64(got))
			case tc.want != nil && (got == nil || !near(*got, *tc.want)):
				t.Fatalf("cache share = %v, want %v", f64(got), f64(tc.want))
			}
		})
	}
}

// Saturated token totals keep the share a finite number from 0 to 1.
func TestCacheShareStaysAShareWhenTotalsSaturate(t *testing.T) {
	t.Parallel()
	const maxCount = 1 << 53
	var sessions []archive.Metadata
	for i := range 300 {
		sessions = append(sessions, meta(fmt.Sprintf("s%03d", i), "claude", day(time.September, 10, 9), modelTokens("m", maxCount, maxCount, maxCount, maxCount)))
	}
	got := Compute(sessions, flatOptions()).Overview.CacheShare
	if got == nil || math.IsNaN(*got) || *got < 0 || *got > 1 {
		t.Fatalf("cache share = %v", f64(got))
	}
}
