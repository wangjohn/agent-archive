package cli

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// pageCase is a set of numbers a page is drawn from.
type pageCase struct {
	name  string
	stats stats.Stats
}

func pageCases() []pageCase {
	return []pageCase{
		{"realistic", realisticStats()},
		{"prior", withPrior(realisticStats())},
		{"cursor-only", cursorOnlyStats()},
		{"saturated", saturatedStats()},
		{"hostile", hostileStats()},
	}
}

// pageVariant is how a page is drawn: in color or not, in ASCII or not.
type pageVariant struct {
	color bool
	ascii bool
}

// pageLines draws a page at a width, in color or plain, in Unicode or ASCII.
func pageLines(page statsPage, s stats.Stats, width int, color, ascii bool) []string {
	glyphs := unicodeGlyphs
	if ascii {
		glyphs = asciiGlyphs
	}
	return renderPage(page, s, statsView{style: textStyle{color: color}, width: width, glyphs: glyphs})
}

// pageGolden is a page as a golden file records it: with each escape
// character written as \e so the file is text.
func pageGolden(lines []string) []byte {
	return []byte(strings.ReplaceAll(strings.Join(lines, "\n")+"\n", "\x1b", `\e`))
}

func stripANSI(text string) string { return ansiSequence.ReplaceAllString(text, "") }

// Every page is pinned at the three widths it has layouts for, with and
// without color, from the plan's realistic numbers; the other cases pin the
// layouts they change (a previous period, Cursor alone, saturated sums, hostile
// names) and ASCII has its own.
func TestStatsPageGoldens(t *testing.T) {
	t.Parallel()
	type goldenCase struct {
		name  string
		page  statsPage
		s     stats.Stats
		width int
		color bool
		ascii bool
	}
	var cases []goldenCase
	for _, page := range statsPages {
		for _, width := range []int{60, 80, 120} {
			for _, color := range []bool{false, true} {
				name := fmt.Sprintf("realistic-%s-%d", page, width)
				if color {
					name += ".color"
				}
				cases = append(cases, goldenCase{name, page, realisticStats(), width, color, false})
			}
		}
	}
	for _, page := range []statsPage{pageOverview, pageDetail} {
		for _, color := range []bool{false, true} {
			suffix := ""
			if color {
				suffix = ".color"
			}
			cases = append(cases,
				goldenCase{fmt.Sprintf("prior-%s-80%s", page, suffix), page, withPrior(realisticStats()), 80, color, false},
				goldenCase{fmt.Sprintf("prior-%s-60%s", page, suffix), page, withPrior(realisticStats()), 60, color, false},
			)
		}
		cases = append(cases,
			goldenCase{fmt.Sprintf("saturated-%s-80", page), page, saturatedStats(), 80, false, false},
			goldenCase{fmt.Sprintf("hostile-%s-80", page), page, hostileStats(), 80, false, false},
			goldenCase{fmt.Sprintf("hostile-%s-60", page), page, hostileStats(), 60, false, false},
			goldenCase{fmt.Sprintf("realistic-%s-80.ascii", page), page, realisticStats(), 80, false, true},
		)
	}
	for _, page := range statsPages {
		cases = append(cases, goldenCase{fmt.Sprintf("cursor-only-%s-80", page), page, cursorOnlyStats(), 80, false, false})
	}
	cases = append(cases,
		goldenCase{"realistic-overview-40", pageOverview, realisticStats(), 40, false, false},
		goldenCase{"realistic-detail-40", pageDetail, realisticStats(), 40, false, false},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines := pageLines(tc.page, tc.s, tc.width, tc.color, tc.ascii)
			golden.Check(t, filepath.Join("testdata", "stats", "pages", tc.name+".golden"), pageGolden(lines))
			for _, line := range lines {
				if w := visibleWidth(line); w > tc.width {
					t.Errorf("line is %d columns, over %d: %q", w, tc.width, line)
				}
			}
		})
	}
}

// No line of any page is wider than the terminal, at any width from the
// narrowest the screens adapt to (40) to a very wide terminal (250), for any
// numbers, in color and out, in Unicode and ASCII; and none carries a control
// character or a terminal escape but its own color codes.
func TestStatsPagesNeverPassTheTerminalWidth(t *testing.T) {
	t.Parallel()
	for _, tc := range pageCases() {
		for _, page := range statsPages {
			t.Run(tc.name+"-"+string(page), func(t *testing.T) {
				t.Parallel()
				for width := statsMinWidth; width <= 250; width++ {
					limit := min(width, statsMaxWidth)
					for _, variant := range []pageVariant{{false, false}, {true, false}, {false, true}} {
						lines := pageLines(page, tc.stats, width, variant.color, variant.ascii)
						out := strings.Join(lines, "\n")
						checkTerminalSafe(t, fmt.Sprintf("%s %s width %d %+v", tc.name, page, width, variant), stripANSI(out), limit)
						if !variant.color && strings.Contains(out, "\x1b") {
							t.Fatalf("width %d: escape code in a plain page", width)
						}
					}
				}
			})
		}
	}
}

// Color never changes the text: a colored page with its codes taken out is the
// plain page, at every width.
func TestStatsColorNeverChangesTheText(t *testing.T) {
	t.Parallel()
	for _, tc := range pageCases() {
		for _, page := range statsPages {
			for _, width := range []int{40, 60, 79, 80, 120} {
				plain := strings.Join(pageLines(page, tc.stats, width, false, false), "\n")
				colored := strings.Join(pageLines(page, tc.stats, width, true, false), "\n")
				if got := stripANSI(colored); got != plain {
					t.Errorf("%s %s %d: color changed the text:\n%s\nvs\n%s", tc.name, page, width, got, plain)
				}
			}
		}
	}
}

// A page with color uses only the terminal's 16 ANSI colors (30 to 37 and 90
// to 97), bold and dim: never a 256-color or truecolor code.
func TestStatsPagesUseOnlyANSISixteenColors(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{"0": true, "1": true, "2": true}
	for code := 30; code <= 37; code++ {
		allowed[strconv.Itoa(code)] = true
		allowed[strconv.Itoa(code+60)] = true
	}
	seen := map[string]bool{}
	for _, tc := range pageCases() {
		for _, page := range statsPages {
			for _, width := range []int{60, 80, 120} {
				for _, line := range pageLines(page, tc.stats, width, true, false) {
					for _, m := range ansiSequence.FindAllString(line, -1) {
						code := strings.TrimSuffix(strings.TrimPrefix(m, "\x1b["), "m")
						if !allowed[code] {
							t.Fatalf("%s %s %d: code %q is not one of the 16 ANSI colors, bold or dim: %q", tc.name, page, width, code, line)
						}
						seen[code] = true
					}
				}
			}
		}
	}
	// The screen uses a spread of them; a page that lost its color would
	// pass the check above.
	for _, code := range []string{"1", "2", "32", "33", "34", "35", "36"} {
		if !seen[code] {
			t.Errorf("code %s never appears in any colored page", code)
		}
	}
}

// One color table: agents have their own colors, model families have their own,
// and every code in the table is one of the 16.
func TestStatsColorTableIsDistinctWhereItMustBe(t *testing.T) {
	t.Parallel()
	distinct := func(what string, codes map[string]string) {
		t.Helper()
		byCode := map[string]string{}
		for name, code := range codes {
			if code == "" {
				t.Errorf("%s %s has no color", what, name)
			}
			if other, dup := byCode[code]; dup {
				t.Errorf("%s %s and %s share color %s", what, name, other, code)
			}
			byCode[code] = name
		}
	}
	agents := map[string]string{}
	for _, harness := range []string{"claude", "cursor", "codex"} {
		agents[harness] = agentCode(harness)
	}
	distinct("agent", agents)
	models := map[string]string{}
	for _, family := range []string{"opus", "fable", "sonnet", "haiku", "gpt-5.6"} {
		models[family] = modelCode(family)
	}
	// Codex's models and GPT are one vendor's: one color.
	if modelCode("codex-auto-review") != modelCode("gpt-5") {
		t.Errorf("codex models and gpt models differ: %q vs %q", modelCode("codex-auto-review"), modelCode("gpt-5"))
	}
	distinct("model family", models)
	if modelCode("some-other-model") != "" || agentCode("vim") != "" {
		t.Error("a name the table does not know is colored")
	}
	if modelCode("claude-opus-5") != modelCode("opus") {
		t.Error("a full Claude model id is not colored as its family")
	}
	tokens := map[string]string{}
	for role, name := range map[statsRole]string{roleCacheRead: "cache read", roleCacheWrite: "cache write", roleFreshInput: "input", roleOutput: "output"} {
		tokens[name] = statsRoleCodes[role]
	}
	distinct("token type", tokens)
	// Spend going up is amber and going down is green; never red.
	if statsRoleCodes[roleDeltaUp] != "33" || statsRoleCodes[roleDeltaDown] != "32" {
		t.Errorf("deltas are %s and %s, want amber (33) and green (32)", statsRoleCodes[roleDeltaUp], statsRoleCodes[roleDeltaDown])
	}
	for role, code := range statsRoleCodes {
		if red := map[string]bool{"31": true, "91": true}; red[code] {
			t.Errorf("role %d is red (%s): spend is not an error", role, code)
		}
	}
}

// Color is never the only cue: with no color at all, every agent in the bar
// is named with its share, every token type in the composition is named with
// its share, and the daily chart's unknown days have their own glyph.
func TestStatsColorsHaveNonColorCues(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	overview := strings.Join(pageLines(pageOverview, s, 80, false, false), "\n")
	for _, want := range []string{"● Claude Code 90%", "● Cursor 9%", "● Codex 1%"} {
		if !strings.Contains(overview, want) {
			t.Errorf("the agents legend lacks %q without color:\n%s", want, overview)
		}
	}
	detail := strings.Join(pageLines(pageDetail, s, 80, false, false), "\n")
	for _, want := range []string{"● Cache read    97%", "● Cache write    2%", "● Fresh input", "● Output"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the composition legend lacks %q without color:\n%s", want, detail)
		}
	}
	// The parts of a stacked bar are told apart by a blank cell.
	for line := range strings.SplitSeq(overview, "\n") {
		if strings.HasPrefix(line, "AGENTS") && len(strings.Fields(line)) != 1+3 {
			t.Errorf("the agents bar's three parts are not separated: %q", line)
		}
	}
	// Model rows are named; the family color only repeats what the label says.
	if !strings.Contains(overview, "opus") || !strings.Contains(overview, "fable") {
		t.Errorf("model rows are not named:\n%s", overview)
	}
}

// NO_COLOR and a dumb terminal turn every code off, and so does output that is
// not a terminal at all; the text is the same.
func TestStatsHonorsNoColorAndNonTerminals(t *testing.T) {
	t.Parallel()
	env := func(vars map[string]string) func(string) string {
		return func(key string) string { return vars[key] }
	}
	for name, tc := range map[string]struct {
		vars  map[string]string
		color bool
	}{
		"plain terminal": {nil, true},
		"NO_COLOR":       {map[string]string{"NO_COLOR": "1"}, false},
		"dumb terminal":  {map[string]string{"TERM": "dumb"}, false},
	} {
		style := terminalStyle(env(tc.vars), 80)
		if style.color != tc.color {
			t.Errorf("%s: color=%v, want %v", name, style.color, tc.color)
		}
		for _, page := range statsPages {
			out := strings.Join(renderPage(page, realisticStats(), statsView{style: style, width: 80, glyphs: unicodeGlyphs}), "\n")
			if hasEscape := strings.Contains(out, "\x1b"); hasEscape != tc.color {
				t.Errorf("%s %s: escape codes=%v, want %v", name, page, hasEscape, tc.color)
			}
		}
	}
}

// The daily spend chart draws the estimated cost of each day, three rows
// tall, and marks a day with no sessions differently from one whose spend is
// unknown.
func TestStatsDailySpendChart(t *testing.T) {
	t.Parallel()
	p := func(width int, s stats.Stats) *statsPrinter {
		return newStatsPrinter(s, statsView{width: width, glyphs: unicodeGlyphs})
	}
	// How many columns a day takes: four for a week, two for a month at 80
	// columns, one for a quarter where it fits, and runs of days where it does
	// not.
	type scale struct {
		days   int
		width  int
		perDay int
		run    int
	}
	for _, tc := range []scale{
		{7, 80, 4, 1}, {14, 80, 4, 1}, {30, 80, 2, 1}, {30, 60, 2, 1}, {30, 120, 4, 1}, {30, 40, 1, 1},
		{90, 100, 1, 1}, {90, 80, 1, 2}, {365, 80, 1, 5}, {3660, 120, 1, 31},
	} {
		perDay, run := p(tc.width, stats.Stats{}).chartScale(tc.days)
		if perDay != tc.perDay || run != tc.run {
			t.Errorf("%d days at %d columns: %d columns a day, runs of %d; want %d and %d", tc.days, tc.width, perDay, run, tc.perDay, tc.run)
		}
		if span := chartSpan((tc.days+run-1)/run, perDay); span > tc.width {
			t.Errorf("%d days at %d columns: the chart is %d wide", tc.days, tc.width, span)
		}
	}
	// Heights: the peak is the full 24 levels, a day with any spend is at least
	// two, a day without is the baseline, and an unknown day is not a bar.
	peak := 100.0
	day := func(sessions int, cost *float64) stats.Day {
		return stats.Day{Sessions: sessions, Cost: stats.Cost{USD: cost}}
	}
	for _, tc := range []struct {
		name string
		days []stats.Day
		want spendCell
	}{
		{"peak", []stats.Day{day(3, f64(100))}, spendCell{level: 24}},
		{"half", []stats.Day{day(3, f64(50))}, spendCell{level: 12}},
		{"a little", []stats.Day{day(1, f64(0.01))}, spendCell{level: chartMinLevel}},
		{"no sessions", []stats.Day{day(0, f64(0))}, spendCell{}},
		{"zero spend", []stats.Day{day(2, f64(0))}, spendCell{}},
		{"unknown", []stats.Day{day(2, nil)}, spendCell{unknown: true}},
		{"empty day with no cost", []stats.Day{day(0, nil)}, spendCell{}},
		{"run: the costliest day", []stats.Day{day(1, f64(10)), day(1, f64(100)), day(1, nil)}, spendCell{level: 24}},
		{"run: all unknown", []stats.Day{day(1, nil), day(2, nil)}, spendCell{unknown: true}},
	} {
		if got := spendCellOf(tc.days, peak); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// A week, three rows: a low day, the peak, no sessions, an unknown day.
	s := stats.Stats{
		Window:    stats.Window{Days: 4},
		PeakSpend: &stats.PeakSpend{Date: "2026-09-02", USD: 100},
		Daily: []stats.Day{
			{Date: "2026-09-01", Sessions: 1, Cost: usd(20)}, {Date: "2026-09-02", Sessions: 2, Cost: usd(100)},
			{Date: "2026-09-03"}, {Date: "2026-09-04", Sessions: 1},
		},
	}
	lines := p(80, s).dailySpend()
	// A heading, three rows and the axis, and a caption for the dot.
	if len(lines) != 6 {
		t.Fatalf("the chart is %d lines, want a heading, three rows, the axis and a caption:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if lines[5] != "· spend unknown" {
		t.Errorf("caption: %q", lines[5])
	}
	if !strings.HasPrefix(lines[0], "DAILY SPEND") || !strings.HasSuffix(lines[0], "peak ~$100 · Sep 2") {
		t.Errorf("heading: %q", lines[0])
	}
	if !strings.HasPrefix(lines[4], "Sep 1") || !strings.HasSuffix(lines[4], "Sep 4") {
		t.Errorf("axis: %q", lines[4])
	}
	rows := lines[1:4]
	if strings.Trim(rows[0], " ") != "███" || strings.Trim(rows[1], " ") != "███" {
		t.Errorf("only the peak reaches the top rows: %q", rows)
	}
	bottom := rows[2]
	for _, mark := range []string{"▅", "█", "▁", "·"} {
		if !strings.Contains(bottom, mark) {
			t.Errorf("the bottom row lacks %q: %q", mark, bottom)
		}
	}
	// No priced spend, no chart.
	if lines := p(80, stats.Stats{Daily: s.Daily}).dailySpend(); lines != nil {
		t.Errorf("a chart without a peak: %v", lines)
	}
}

// Tables fit the terminal: the bars shrink first, then labels are cut, then the
// bars go, and last the columns that may be left out; the numbers are never
// cut.
func TestStatsTableFitsTheTerminal(t *testing.T) {
	t.Parallel()
	bar := func() *tableBar {
		return &tableBar{width: 22, shares: []float64{1, 0.5}, codes: []string{"", ""}}
	}
	cols := []tableCol{{head: "total", cells: []string{"123456789012", "5"}}}
	for _, width := range []int{80, 60} {
		p := &statsPrinter{width: width, g: unicodeGlyphs}
		lines := p.table("T", []string{"x", "y"}, bar(), cols)
		if !strings.Contains(lines[1], "█") {
			t.Errorf("width %d: no bar: %q", width, lines[1])
		}
		for _, line := range lines {
			if w := visibleWidth(line); w > width {
				t.Errorf("width %d: %d columns: %q", width, w, line)
			}
		}
		if !strings.Contains(lines[1], "123456789012") {
			t.Errorf("width %d: the number was cut: %q", width, lines[1])
		}
	}
	// The bar shrinks before the number is cut.
	p := &statsPrinter{width: 60, g: unicodeGlyphs}
	long := []tableCol{{head: "total", cells: []string{strings.Repeat("9", 40), "5"}}}
	lines := p.table("T", []string{"x", "y"}, bar(), long)
	if n := strings.Count(lines[1], "█"); n >= 22 || n < minBarWidth || !strings.Contains(lines[1], long[0].cells[0]) {
		t.Errorf("the bar did not shrink to fit, or the number was cut: %q", lines[1])
	}
	// Narrower than 60 columns a table has no bars, whatever it is asked for.
	p = &statsPrinter{width: 50, g: unicodeGlyphs}
	if lines := p.table("T", []string{"x", "y"}, bar(), cols); strings.Contains(strings.Join(lines, ""), "█") {
		t.Errorf("a bar under 60 columns: %v", lines)
	}
	// A table too wide for its columns drops the droppable ones, last first,
	// and keeps the label and the numbers.
	wide := []tableCol{
		{head: "sessions", cells: []string{"12", "3"}},
		{head: "share", cells: []string{"50%", "5%"}, drop: true},
		{head: "tokens", cells: []string{"4.5M", "10K"}},
		{head: "est. cost", cells: []string{"$1,234", "$5"}},
		{head: "cache hit", cells: []string{"97%", "90%"}, drop: true},
	}
	for _, tc := range []struct {
		width    int
		wantHead []string
		absent   []string
	}{
		{80, []string{"sessions", "share", "tokens", "est. cost", "cache hit"}, nil},
		{50, []string{"sessions", "tokens", "est. cost"}, []string{"cache hit"}},
		{40, []string{"sessions", "tokens", "est. cost"}, []string{"cache hit", "share"}},
	} {
		p := &statsPrinter{width: tc.width, g: unicodeGlyphs}
		lines := p.table("AGENTS", []string{"Claude Code", "Cursor"}, nil, wide)
		for _, line := range lines {
			if w := visibleWidth(line); w > tc.width {
				t.Errorf("width %d: %d columns: %q", tc.width, w, line)
			}
		}
		for _, h := range tc.wantHead {
			if !strings.Contains(lines[0], h) {
				t.Errorf("width %d: lost the %q column: %q", tc.width, h, lines[0])
			}
		}
		for _, h := range tc.absent {
			if strings.Contains(lines[0], h) {
				t.Errorf("width %d: kept the %q column: %q", tc.width, h, lines[0])
			}
		}
	}
	// A label that would push a row past the terminal is cut with an ellipsis.
	p = &statsPrinter{width: 40, g: unicodeGlyphs}
	lines = p.table("PROJECTS", []string{strings.Repeat("x", 60), "short"}, nil, []tableCol{
		{head: "sessions", cells: []string{"12", "3"}}, {head: "tokens", cells: []string{"4.5M", "10K"}}, {head: "est. cost", cells: []string{"$1,234", "$5"}},
	})
	if !strings.Contains(lines[1], "…") || !strings.Contains(lines[1], "$1,234") {
		t.Errorf("a long label was not cut, or the numbers were: %q", lines[1])
	}
}

// A bar has no track behind it: a filled part and blanks, and a negative
// share is all blanks.
func TestStatsBarsHaveNoTrack(t *testing.T) {
	t.Parallel()
	p := &statsPrinter{g: unicodeGlyphs}
	for _, tc := range []struct {
		share float64
		want  string
	}{
		{-1, "      "}, {0, "      "}, {0.5, "███   "}, {0.01, "█     "}, {1, "██████"}, {7, "██████"},
	} {
		if got := p.bar(tc.share, 6, ""); got != tc.want {
			t.Errorf("bar(%v) = %q, want %q", tc.share, got, tc.want)
		}
	}
	for _, page := range statsPages {
		for _, line := range pageLines(page, realisticStats(), 120, false, false) {
			if strings.ContainsAny(line, "░▒▓") {
				t.Errorf("%s has a shaded track or shade: %q", page, line)
			}
		}
	}
}

// A stacked bar fills its width exactly, gives a part with any share a cell, and
// puts a blank between the parts.
func TestStatsStackedBarFillsItsWidth(t *testing.T) {
	t.Parallel()
	p := &statsPrinter{g: unicodeGlyphs}
	for _, tc := range []struct {
		shares []float64
		width  int
	}{
		{[]float64{0.9, 0.09, 0.01}, 50}, {[]float64{1}, 30}, {[]float64{0.97, 0.02, 0.003, 0.007}, 40}, {[]float64{0.5, 0.5}, 21},
	} {
		segments := make([]segment, len(tc.shares))
		for i, s := range tc.shares {
			segments[i] = segment{share: s}
		}
		bar := p.stackedBar(segments, tc.width)
		if got := visibleWidth(bar); got != tc.width {
			t.Errorf("%v in %d: %d wide: %q", tc.shares, tc.width, got, bar)
		}
		if parts := strings.Fields(bar); len(parts) != len(tc.shares) {
			t.Errorf("%v: %d parts in %q", tc.shares, len(parts), bar)
		}
	}
}

// The overview shows its lists whole when they are one row past the limit,
// and says how many more otherwise.
func TestStatsOverviewListLimits(t *testing.T) {
	t.Parallel()
	type limit struct {
		n     int
		limit int
		want  int
	}
	for _, tc := range []limit{{0, 4, 0}, {4, 4, 4}, {5, 4, 5}, {6, 4, 4}, {10, 3, 3}, {4, 3, 4}} {
		if got := limitRows(tc.n, tc.limit); got != tc.want {
			t.Errorf("limitRows(%d, %d) = %d, want %d", tc.n, tc.limit, got, tc.want)
		}
	}
	out := strings.Join(pageLines(pageOverview, realisticStats(), 80, false, false), "\n")
	for _, want := range []string{"+ 6 more", "+ 2 more"} {
		if !strings.Contains(out, want) {
			t.Errorf("the overview does not say %q:\n%s", want, out)
		}
	}
}

// The overview's spec, line by line, at 80 columns from the plan's numbers.
func TestStatsOverviewFollowsTheSpec(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	out := strings.Join(pageLines(pageOverview, s, 80, false, false), "\n")
	for _, want := range []string{
		"agent-archive stats · last 30 days · 3 agents",
		"~$3,989", "93 sessions", "10B tokens", "673 prompts", "97% served from cache",
		"● Claude Code 90%   ● Cursor 9%   ● Codex 1%   of sessions",
		"peak ~$2,910 · Sep 27", "Aug 31", "Sep 29",
		"By project", "By model", "agent-archive", "+ 6 more", "opus",
		"Skills  code-review 10 · review-pr 4 · docs 3 · cursor-guide 2 sessions",
		"MCP     github 41 · linear 12 calls (Claude Code and Cursor only)",
		"● 77% of tokens came from subagents (497 runs)",
		"● Costliest session ~$564 · styleprofile · long context, 38 subagents",
		"● 10 sessions have no token data (Cursor 8, Claude Code 2)",
		"Estimated at list price, not a bill.   --detail for more · --by project · --html",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the overview lacks %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"anthropic-skills:", "Active days", "streak", "Busiest day", "Favorite", "Tool errors", "TOKENS BY DAY", "new", "vs prior", "sessions)", "subagent sessions"} {
		if strings.Contains(out, gone) {
			t.Errorf("the overview still has %q:\n%s", gone, out)
		}
	}
	// A delta only when there is a previous period, and only then.
	with := strings.Join(pageLines(pageOverview, withPrior(s), 80, false, false), "\n")
	if !strings.Contains(with, "▲ 18% vs prior 30d") {
		t.Errorf("no delta with a previous period:\n%s", with)
	}
	// A previous period of zero has no percentage: nothing, never "new".
	zero := withPrior(s)
	zero.Overview.Cost.Previous, zero.Overview.Cost.ChangePct = f64(0), nil
	if out := strings.Join(pageLines(pageOverview, zero, 80, false, false), "\n"); strings.Contains(out, "vs prior") || strings.Contains(out, "new") {
		t.Errorf("a previous of zero shows a change:\n%s", out)
	}
	// Spend up is amber and down is green.
	up := strings.Join(pageLines(pageOverview, withPrior(s), 80, true, false), "\n")
	if !strings.Contains(up, "\x1b[33m▲ 18%\x1b[0m") {
		t.Errorf("spend up is not amber:\n%q", up)
	}
	down := withPrior(s)
	down.Overview.Cost.Previous, down.Overview.Cost.ChangePct = f64(5000), f64(-20.2)
	if out := strings.Join(pageLines(pageOverview, down, 80, true, false), "\n"); !strings.Contains(out, "\x1b[32m▼ 20%\x1b[0m") {
		t.Errorf("spend down is not green:\n%q", out)
	}
	// The headline's "+" says unpriced tokens were left out.
	partial := s
	partial.Overview.Cost.Partial = true
	if out := strings.Join(pageLines(pageOverview, partial, 80, false, false), "\n"); !strings.Contains(out, "~$3,989+") || !strings.Contains(out, "+ leaves out models with no price.") {
		t.Errorf("a partial cost is not marked:\n%s", out)
	}
}

// The detail screen has what the overview leaves out.
func TestStatsDetailHasWhatTheOverviewLeavesOut(t *testing.T) {
	t.Parallel()
	out := flatten(strings.Join(pageLines(pageDetail, withPrior(realisticStats()), 100, false, false), "\n"))
	for _, want := range []string{
		"OVERVIEW", "last 30d", "prior 30d", "change", "3 of 30", "97% of tokens were served from cache",
		"Streak", "Busiest day", "Sep 27 (70 sessions)", "Favorite model", "opus", "This month", "heaviest of the last 6 months",
		"Tool errors", "5.0% of 4,200 tool results flagged as errors", "13 do not record them",
		"WHAT USED YOUR TOKENS", "Cache read", "Cache write", "Fresh input", "Output", "Subagents 77% of tokens (7.7B) in 497 runs",
		"AGENTS", "cache hit", "SKILLS AND MCP", "NOTES", "Scope: this archive only.", "83 of 93 sessions report token counts.",
		"Prices 2026-09-29", "~ marks an estimate at list price.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the detail screen lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "subagent sessions") {
		t.Errorf("subagent runs are called sessions:\n%s", out)
	}
}

// Projects, models and agents list every row, and the agents screen says what
// Cursor and Codex do not record.
func TestStatsListScreens(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	projects := strings.Join(pageLines(pageProjects, s, 100, false, false), "\n")
	for _, p := range s.Projects {
		if !strings.Contains(projects, p.Name) {
			t.Errorf("the projects screen lacks %q:\n%s", p.Name, projects)
		}
	}
	for _, want := range []string{"PROJECTS (10)", "47%", "unknown", "n/a"} {
		if !strings.Contains(projects, want) {
			t.Errorf("the projects screen lacks %q:\n%s", want, projects)
		}
	}
	models := strings.Join(pageLines(pageModels, s, 100, false, false), "\n")
	for _, m := range s.Models {
		if !strings.Contains(models, m.Label) {
			t.Errorf("the models screen lacks %q:\n%s", m.Label, models)
		}
	}
	unpriced := s
	unpriced.Models = append(append([]stats.ModelRow(nil), s.Models...), stats.ModelRow{Label: "mystery-1", Sessions: 1, Tokens: 5000})
	if out := strings.Join(pageLines(pageModels, unpriced, 100, false, false), "\n"); !strings.Contains(out, "unpriced") || !strings.Contains(out, "mystery-1") || !strings.Contains(out, "Unpriced: the price table does not list mystery-1, so its tokens") {
		t.Errorf("an unpriced model is not flagged:\n%s", out)
	}
	agents := strings.Join(pageLines(pageAgents, s, 100, false, false), "\n")
	for _, want := range []string{"Claude Code", "Cursor", "Codex", "unknown", "97%", "Cursor: 8 of 8 have no token data", "Codex: MCP calls and tool errors are not recorded.", "Cache hit is cache reads"} {
		if !strings.Contains(agents, want) {
			t.Errorf("the agents screen lacks %q:\n%s", want, agents)
		}
	}
	// A window without model data says so instead of drawing an empty table.
	if out := strings.Join(pageLines(pageModels, cursorOnlyStats(), 80, false, false), "\n"); !strings.Contains(out, "No session in this window reports tokens by model") {
		t.Errorf("Cursor alone:\n%s", out)
	}
}

// Every list screen is bounded, so a window with thousands of projects still
// prints a page a person can read, and says how many it left out.
func TestStatsListScreensAreBounded(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	s.Projects = nil
	for i := range statsMaxListRows + 40 {
		s.Projects = append(s.Projects, stats.Project{Name: fmt.Sprintf("p%04d", i), Sessions: 1, Tokens: i64(int64(1000 - i%900)), Cost: usd(float64(1000 - i%900))})
	}
	s.TotalProjects = len(s.Projects)
	lines := pageLines(pageProjects, s, 80, false, false)
	if len(lines) > statsMaxListRows+10 {
		t.Errorf("%d lines for %d projects", len(lines), len(s.Projects))
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "+ 40 more (--json has them all)") {
		t.Errorf("the screen does not say what it left out:\n%s", lines[len(lines)-4:])
	}
}

// Names are safe to print and every project, model, skill, MCP server and agent
// name is cut to its column: the hostile numbers' pages carry none of them raw.
func TestStatsHostileNamesInPages(t *testing.T) {
	t.Parallel()
	for _, page := range statsPages {
		for _, width := range []int{40, 60, 80, 120} {
			out := strings.Join(pageLines(page, hostileStats(), width, true, false), "\n")
			checkTerminalSafe(t, fmt.Sprintf("%s %d", page, width), stripANSI(out), width)
		}
	}
}

// A float past the int64 range is shown as the same bound on every platform
// (converting it directly is left to the CPU: amd64 wraps, arm64 saturates).
func TestStatsRoundIntSaturates(t *testing.T) {
	t.Parallel()
	type roundCase struct {
		in   float64
		want int64
	}
	for _, tc := range []roundCase{
		{0, 0}, {2.5, 3}, {-2.5, -3}, {1e18, 1e18}, {math.MaxInt64, math.MaxInt64}, {1e30, math.MaxInt64},
		{math.Inf(1), math.MaxInt64}, {-1e30, math.MinInt64}, {math.Inf(-1), math.MinInt64}, {math.NaN(), 0},
	} {
		if got := statsfmt.RoundInt(tc.in); got != tc.want {
			t.Errorf("RoundInt(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// A change of more than the int64 range reads as the same bound as any
	// other rise past 999%, never wraps.
	p := newStatsPrinter(stats.Stats{}, statsView{glyphs: unicodeGlyphs})
	huge := stats.Measure{Value: f64(1e30), Previous: f64(1), ChangePct: f64(1e32)}
	if got := p.changeText(huge); got != "▲ >999%" {
		t.Errorf("a huge rise reads %q", got)
	}
	huge.ChangePct = f64(-1e32)
	if got := p.changeText(huge); got != "▼ >999%" {
		t.Errorf("a huge fall reads %q", got)
	}
}

// A rise from next to nothing reads ">999%" on the headline and in the
// detail table, not as the millions of percent it is; 999% still reads as it
// is. A rise that is only large is a number.
func TestStatsChangeIsCappedWhereItIsArithmetic(t *testing.T) {
	t.Parallel()
	p := newStatsPrinter(stats.Stats{}, statsView{glyphs: unicodeGlyphs})
	for _, tc := range []struct {
		pct  float64
		want string
	}{
		{25_219_191, "▲ >999%"}, {1000, "▲ >999%"}, {999, "▲ 999%"}, {5_900, "▲ >999%"}, {64, "▲ 64%"},
		{-100, "▼ 100%"}, {-40, "▼ 40%"}, {0.2, "no change"},
	} {
		m := stats.Measure{Value: f64(10), Previous: f64(1), ChangePct: f64(tc.pct)}
		if got := p.changeText(m); got != tc.want {
			t.Errorf("a change of %v%% reads %q, want %q", tc.pct, got, tc.want)
		}
	}
	m := stats.Measure{Value: f64(3), Previous: f64(1), ChangePct: f64(25_219_191)}
	if got := p.deltaText(m); !strings.HasPrefix(got, "▲ >999% vs prior") {
		t.Errorf("the headline change reads %q", got)
	}
}
