package cli

import (
	"math"
	"strings"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// The daily spend chart is three rows of block characters: 8 levels a row, so
// 24 levels from the baseline to the peak.
const (
	chartRows   = 3
	chartLevels = chartRows * 8
	// chartMinLevel is the lowest a day with any spend is drawn, so it reads
	// as more than a day without: a day without sessions is the baseline
	// alone (level 0), and a day with a little spend is a mark above it.
	chartMinLevel = 2
	// chartMaxDayWidth is the most columns a day takes.
	chartMaxDayWidth = 4
)

// spendCell is one column group of the chart: a day, or a run of days when
// the window has more days than the terminal has columns.
type spendCell struct {
	// level is the bar's height, 0 to chartLevels; 0 is a day without spend.
	level int
	// unknown marks a day whose sessions have no cost: no token counts, or
	// only models the price table lacks.
	unknown bool
}

// dailySpend is the estimated cost of each day of the window as a bar chart,
// three rows tall, with the peak day named. A day takes as many columns as
// fit (four at most, one of them a gap between neighbors; at one column a day
// there is no gap), and a window with more days than columns draws each cell
// as the costliest day of a run. Days without sessions are a baseline, and days
// whose spend is unknown are a dot, never a low bar. No chart is drawn when no
// day has any priced spend.
func (p *statsPrinter) dailySpend() []string {
	s := p.s
	if len(s.Daily) == 0 || s.PeakSpend == nil {
		return nil
	}
	perDay, run := p.chartScale(len(s.Daily))
	cells := p.spendCells(run)
	span := chartSpan(len(cells), perDay)
	note := p.dim("peak ") + p.role(roleHeadsUp, p.estimate(s.PeakSpend.USD)) + p.dim(" "+p.g.sep+" "+p.dayLabel(s.PeakSpend.Date))
	title := "DAILY SPEND"
	edge := max(span, visibleWidth(title)+2+visibleWidth(note))
	lines := []string{p.heading(title, note, edge)}
	lines = append(lines, p.chartRows(cells, perDay)...)
	first, last := p.dayLabel(s.Daily[0].Date), p.dayLabel(s.Daily[len(s.Daily)-1].Date)
	labels := first
	if gap := span - visibleWidth(first) - visibleWidth(last); len(s.Daily) > 1 && gap >= 1 {
		labels += strings.Repeat(" ", gap) + last
	}
	return append(lines, p.dim(labels))
}

// chartScale is how many columns a day takes and how many days a cell
// covers: a day takes the widest of four columns down to one that fits the
// terminal; when even one column each is too wide, a cell covers a run.
func (p *statsPrinter) chartScale(days int) (perDay, run int) {
	for perDay = chartMaxDayWidth; perDay > 1; perDay-- {
		if chartSpan(days, perDay) <= p.width {
			return perDay, 1
		}
	}
	run = 1
	for (days+run-1)/run > p.width {
		run++
	}
	return 1, run
}

// chartSpan is the columns of n cells that take perDay columns each, without
// a gap after the last.
func chartSpan(n, perDay int) int {
	if perDay <= 1 {
		return n
	}
	return n*perDay - 1
}

// spendCells is the chart's cells: each day's spend against the peak, or, in
// runs of several days, the costliest day of the run.
func (p *statsPrinter) spendCells(run int) []spendCell {
	days := p.s.Daily
	peak := p.s.PeakSpend.USD
	var cells []spendCell
	for i := 0; i < len(days); i += run {
		cells = append(cells, spendCellOf(days[i:min(i+run, len(days))], peak))
	}
	return cells
}

// spendCellOf is a run of days as one cell: unknown when every day with
// sessions is, else as high as its costliest day.
func spendCellOf(days []stats.Day, peak float64) spendCell {
	var best float64
	known, unknown := false, false
	for _, d := range days {
		switch {
		case d.Cost.USD != nil:
			known = true
			best = math.Max(best, *d.Cost.USD)
		case d.Sessions > 0:
			unknown = true
		}
	}
	switch {
	case !known && unknown:
		return spendCell{unknown: true}
	case best <= 0 || peak <= 0:
		return spendCell{}
	}
	level := int(math.Round(best / peak * chartLevels))
	return spendCell{level: min(max(level, chartMinLevel), chartLevels)}
}

// chartRows draws the cells top row to bottom, each day perDay columns wide
// (its bar perDay-1 of them when it has a gap).
func (p *statsPrinter) chartRows(cells []spendCell, perDay int) []string {
	barW := max(perDay-1, 1)
	rows := make([]string, chartRows)
	for r := range chartRows {
		fromBottom := chartRows - 1 - r
		var row strings.Builder
		var pending strings.Builder // blanks, kept only if a mark follows
		for i, c := range cells {
			glyph, code := p.chartGlyph(c, fromBottom)
			if glyph == " " {
				pending.WriteString(strings.Repeat(" ", barW))
			} else {
				row.WriteString(pending.String())
				pending.Reset()
				row.WriteString(p.paint(code, strings.Repeat(glyph, barW)))
			}
			if perDay > 1 && i < len(cells)-1 {
				// The bottom row is one unbroken baseline under the gaps too.
				if fromBottom == 0 {
					pending.WriteString(p.paint(statsRoleCodes[roleLabel], p.g.spark[0]))
				} else {
					pending.WriteString(" ")
				}
			}
		}
		rows[r] = row.String()
	}
	return rows
}

// chartGlyph is the character a cell has in a row (0 is the bottom row) and
// the code it is painted in.
func (p *statsPrinter) chartGlyph(c spendCell, fromBottom int) (glyph, code string) {
	switch {
	case c.unknown:
		if fromBottom == 0 {
			return p.g.sparkUnknown, statsRoleCodes[roleLabel]
		}
		return " ", ""
	case c.level == 0:
		if fromBottom == 0 {
			return p.g.spark[0], statsRoleCodes[roleLabel]
		}
		return " ", ""
	}
	eighths := min(max(c.level-8*fromBottom, 0), 8)
	if eighths == 0 {
		return " ", ""
	}
	return p.g.spark[eighths-1], statsRoleCodes[roleSpendChart]
}
