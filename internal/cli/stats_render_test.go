package cli

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
)

func TestStatsNumberFormats(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{
		0: "0", 812: "812", 999: "999", 1000: "1K", 1234: "1.2K", 9949: "9.9K", 9950: "10K", 51_400: "51K", 999_499: "999K", 999_500: "1M", 999_999: "1M", 999_600_000: "1B", 999_500_000_000_000: ">999T",
		1_000_000: "1M", 4_900_000: "4.9M", 61_000_000: "61M", 1_200_000_000: "1.2B", 3_000_000_000_000: "3T",
		// A saturated sum is a bound, not a measurement.
		math.MaxInt64: ">999T",
	} {
		if got := tokenCount(n); got != want {
			t.Errorf("tokenCount(%d) = %q, want %q", n, got, want)
		}
	}
	for _, tc := range []struct {
		amount  float64
		precise bool
		want    string
	}{
		{0, false, "$0.00"}, {0.126, false, "$0.13"}, {5.69, false, "$5.69"}, {9.999, false, "$10.00"}, {59.4, false, "$59"},
		{612.5, false, "$613"}, {41.2, true, "$41.20"}, {1204.5, false, "$1,205"}, {1204.5, true, "$1,204.50"},
		{1e13, false, "$>999B"}, {math.MaxFloat64, true, "$>999B"},
	} {
		if got := money("USD", tc.amount, tc.precise); got != tc.want {
			t.Errorf("money(%v, precise=%v) = %q, want %q", tc.amount, tc.precise, got, tc.want)
		}
	}
	if got := money("EUR", 12.4, false); got != "EUR 12" {
		t.Errorf("money in EUR = %q", got)
	}
	for share, want := range map[float64]string{0: "0%", 0.004: "<1%", 0.005: "1%", 0.184: "18%", 0.995: "100%", 1: "100%"} {
		if got := percent(share); got != want {
			t.Errorf("percent(%v) = %q, want %q", share, got, want)
		}
	}
	for rate, want := range map[float64]string{0: "0%", 0.041: "4.1%", 0.0995: "10.0%", 0.25: "25%"} {
		if got := ratePercent(rate); got != want {
			t.Errorf("ratePercent(%v) = %q, want %q", rate, got, want)
		}
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 3204: "3,204", 1234567: "1,234,567", -5000: "-5,000"} {
		if got := commaInt(n); got != want {
			t.Errorf("commaInt(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// apportion splits a bar into whole cells that add up to the bar, with a
// cell for every segment that has any share.
func TestApportionFillsTheBarExactly(t *testing.T) {
	t.Parallel()
	for _, shares := range [][]float64{
		{0.75, 0.06, 0.13, 0.06}, {1, 0, 0, 0}, {0.999, 0.0005, 0.0003, 0.0002}, {0.25, 0.25, 0.25, 0.25}, {0.333, 0.333, 0.334, 0},
	} {
		cells := apportion(shares, 50)
		sum := 0
		for i, n := range cells {
			sum += n
			if shares[i] > 0 && n == 0 {
				t.Errorf("%v: segment %d has no cell: %v", shares, i, cells)
			}
			if shares[i] == 0 && n != 0 {
				t.Errorf("%v: empty segment %d has %d cells", shares, i, n)
			}
		}
		if sum != 50 {
			t.Errorf("%v: cells %v add up to %d, want 50", shares, cells, sum)
		}
	}
}

func daysOf(n int, tokens func(i int) *int64) []stats.Day {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]stats.Day, n)
	for i := range out {
		out[i] = stats.Day{Date: start.AddDate(0, 0, i).Format("2006-01-02"), Tokens: tokens(i)}
	}
	return out
}

// A long window is drawn in fewer columns than it has days, by summing runs
// of days, so the sparkline never runs past the terminal; nothing is drawn
// taller than the busiest.
func TestSparklineFitsTheTerminal(t *testing.T) {
	t.Parallel()
	one := func(int) *int64 { v := int64(1000); return &v }
	for _, width := range []int{50, 80, 100} {
		p := &statsPrinter{s: stats.Stats{Daily: daysOf(365, one)}, g: unicodeGlyphs, width: width}
		line, span := p.sparkline()
		if span > width-2 || len([]rune(line)) != span {
			t.Errorf("width %d: span %d, line of %d cells", width, span, len([]rune(line)))
		}
		if strings.Trim(line, "█") != "" {
			t.Errorf("width %d: equal days should all be full height: %q", width, line)
		}
	}
	// A day without token data is marked, not drawn as a low bar.
	unknown := func(i int) *int64 {
		if i%2 == 0 {
			return nil
		}
		v := int64(5)
		return &v
	}
	p := &statsPrinter{s: stats.Stats{Daily: daysOf(4, unknown)}, g: unicodeGlyphs, width: 80}
	if line, _ := p.sparkline(); line != "·█·█" {
		t.Errorf("unknown days: %q", line)
	}
	// A run of days with no token data at all is all unknown; a day with
	// tokens 0 is the lowest bar.
	zero := func(i int) *int64 { v := int64(0); return &v }
	p = &statsPrinter{s: stats.Stats{Daily: daysOf(3, zero)}, g: unicodeGlyphs, width: 80}
	if line, _ := p.sparkline(); line != "▁▁▁" {
		t.Errorf("zero days: %q", line)
	}
}

// A table that would run past the terminal cuts its labels, not its numbers.
func TestStatsTableCutsLongLabelsToFit(t *testing.T) {
	t.Parallel()
	p := &statsPrinter{width: 50, g: unicodeGlyphs}
	lines := p.table("TOP PROJECTS", []string{strings.Repeat("x", 60), "short"}, nil, []tableCol{
		{"sessions", []string{"12", "3"}}, {"tokens", []string{"4.5M", "10K"}}, {"est. cost", []string{"$1,234", "$5"}},
	})
	for _, line := range lines {
		if w := visibleWidth(line); w > 50 {
			t.Errorf("%d columns: %q", w, line)
		}
	}
	if !strings.Contains(lines[1], "4.5M") || !strings.Contains(lines[1], "$1,234") {
		t.Errorf("numbers were cut: %q", lines[1])
	}
}

// A window with no days worth drawing (all zero) and one where the stats
// say nothing about tokens skip the sparkline rather than draw nothing.
func TestStatsTokensByDaySkippedWithoutTokenData(t *testing.T) {
	t.Parallel()
	p := &statsPrinter{s: stats.Stats{Daily: daysOf(3, func(int) *int64 { return nil })}, g: unicodeGlyphs, width: 80}
	if lines := p.tokensByDay(); len(lines) != 0 {
		t.Errorf("drew %v", lines)
	}
}
