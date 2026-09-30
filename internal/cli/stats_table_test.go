package cli

import (
	"strings"
	"testing"
)

// A table with bars fits its terminal: the bars shrink first (down to a
// least width), and when even that does not leave room for the numbers, the
// bars go and the table is the compact one. No row is wider than the
// terminal, and the numbers are never cut.
func TestStatsTableShrinksBarsBeforeDroppingThem(t *testing.T) {
	t.Parallel()
	bar := func() *tableBar { return &tableBar{width: 22, shares: []float64{1, 0.5}} }
	cols := []tableCol{{"total", []string{"123456789012", "5"}}}
	for _, tc := range []struct {
		width    int
		wantBars int
	}{
		{80, 22},
		{39, 22},
		{30, 13},
		{27, 10},
		{23, minBarWidth},
		{20, 0},
	} {
		p := &statsPrinter{width: tc.width, g: unicodeGlyphs}
		lines := p.table("T", []string{"x", "y"}, bar(), cols)
		for _, line := range lines {
			if w := visibleWidth(line); w > tc.width {
				t.Errorf("width %d: %d columns: %q", tc.width, w, line)
			}
		}
		if got := strings.Count(lines[1], "█") + strings.Count(lines[1], "░"); got != tc.wantBars {
			t.Errorf("width %d: first bar has %d cells, want %d: %q", tc.width, got, tc.wantBars, lines[1])
		}
		if !strings.Contains(lines[1], "123456789012") {
			t.Errorf("width %d: the number was cut: %q", tc.width, lines[1])
		}
	}
}

// A negative share is a blank of the bar's width, not an empty bar.
func TestStatsBarBlankForNoShare(t *testing.T) {
	t.Parallel()
	p := &statsPrinter{g: unicodeGlyphs}
	if got := p.bar(-1, 6); got != "      " {
		t.Errorf("blank bar = %q", got)
	}
	if got := p.bar(0.5, 6); got != "███░░░" {
		t.Errorf("half bar = %q", got)
	}
	if got := p.bar(0.01, 6); got != "█░░░░░" {
		t.Errorf("a nonzero share draws a cell: %q", got)
	}
}
