package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
)

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
