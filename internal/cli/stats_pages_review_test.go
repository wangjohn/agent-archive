package cli

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// dailyRun is n days ending on Sep 29 with each day's spend given by cost
// (nil for a day with sessions and no price); the peak is the costliest.
func dailyRun(n int, cost func(i int) *float64) stats.Stats {
	s := realisticStats()
	end := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	s.Window.Days = n
	s.Daily = make([]stats.Day, n)
	s.PeakSpend = nil
	for i := range s.Daily {
		c := cost(i)
		day := stats.Day{Date: end.AddDate(0, 0, i-n+1).Format("2006-01-02"), Sessions: 1}
		if c != nil {
			day.Cost = usd(*c)
			if s.PeakSpend == nil || *c > s.PeakSpend.USD {
				s.PeakSpend = &stats.PeakSpend{Date: day.Date, USD: *c}
			}
		}
		s.Daily[i] = day
	}
	return s
}

// A bar that stands for several days says so, and what a dot is; a chart with
// one bar a day and no dot needs no caption.
func TestStatsChartCaptionsSayWhatARunAndADotAre(t *testing.T) {
	t.Parallel()
	spend := func(i int) *float64 { return f64(float64(10 + i%7)) }
	captionOf := func(s stats.Stats, width int) string {
		lines := newStatsPrinter(s, statsView{width: width, glyphs: unicodeGlyphs}).dailySpend()
		if len(lines) == 5 {
			return ""
		}
		if len(lines) != 6 {
			t.Fatalf("%d lines: %q", len(lines), lines)
		}
		return lines[5]
	}
	if got := captionOf(dailyRun(30, spend), 80); got != "" {
		t.Errorf("a month at one bar a day has a caption: %q", got)
	}
	if got := captionOf(dailyRun(90, spend), 100); got != "" {
		t.Errorf("a quarter at 100 columns has a caption: %q", got)
	}
	if got, want := captionOf(dailyRun(90, spend), 80), "each bar is the costliest of 2 days"; got != want {
		t.Errorf("a quarter at 80 columns: caption %q, want %q", got, want)
	}
	// Twenty days in a row with sessions and no price.
	unknown := func(i int) *float64 {
		if i >= 100 && i < 120 {
			return nil
		}
		return spend(i)
	}
	everyFifth := func(i int) *float64 {
		if i%5 == 0 {
			return nil
		}
		return spend(i)
	}
	if got, want := captionOf(dailyRun(30, everyFifth), 80), "· spend unknown"; got != want {
		t.Errorf("unknown days: caption %q, want %q", got, want)
	}
	if got, want := captionOf(dailyRun(365, unknown), 100), "each bar is the costliest of 4 days · · spend unknown"; got != want {
		t.Errorf("a year with unknown days: caption %q, want %q", got, want)
	}
}

// A chart too narrow to put its first and last dates apart still names both.
func TestStatsChartAxisNamesBothEnds(t *testing.T) {
	t.Parallel()
	s := dailyRun(2, func(i int) *float64 { return f64(float64(5 + i)) })
	lines := newStatsPrinter(s, statsView{width: 80, glyphs: unicodeGlyphs}).dailySpend()
	axis := lines[len(lines)-1]
	if !strings.Contains(axis, "Sep 28") || !strings.Contains(axis, "Sep 29") {
		t.Errorf("the axis of a two-day chart is %q", axis)
	}
}

// The projects are ranked by spend, then tokens, sessions and name, whatever
// order they are given in, with the ones that have no price last.
func TestStatsProjectsAreRankedBySpendWhateverTheirOrder(t *testing.T) {
	t.Parallel()
	in := []stats.Project{
		{Name: "unpriced-small", Sessions: 9, Tokens: i64(10)},
		{Name: "tie-b", Sessions: 1, Tokens: i64(500), Cost: usd(50)},
		{Name: "cheap-huge", Sessions: 1, Tokens: i64(9_000_000), Cost: usd(5)},
		{Name: "unpriced-big", Sessions: 1, Tokens: i64(1000)},
		{Name: "top", Sessions: 1, Tokens: i64(1), Cost: usd(90)},
		{Name: "tie-a", Sessions: 1, Tokens: i64(500), Cost: usd(50)},
		{Name: "tie-more-tokens", Sessions: 1, Tokens: i64(600), Cost: usd(50)},
		{Name: "no-tokens", Sessions: 3},
	}
	want := []string{"top", "tie-more-tokens", "tie-a", "tie-b", "cheap-huge", "unpriced-big", "unpriced-small", "no-tokens"}
	names := func(projects []stats.Project) []string {
		var out []string
		for _, p := range projects {
			out = append(out, p.Name)
		}
		return out
	}
	for shift := range in {
		rotated := append(slices.Clone(in[shift:]), in[:shift]...)
		if got := names(projectsBySpend(rotated)); !slices.Equal(got, want) {
			t.Fatalf("from %v: %v, want %v", names(rotated), got, want)
		}
	}
	rev := slices.Clone(in)
	slices.Reverse(rev)
	if got := names(projectsBySpend(rev)); !slices.Equal(got, want) {
		t.Errorf("reversed: %v, want %v", got, want)
	}
}

// The overview, the projects screen and the models screen mark a spend that
// leaves out unpriced models the same way, with the "+" the headline has.
func TestStatsPartialSpendIsMarkedOnEveryScreen(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	s.Overview.Cost.Partial = true
	s.Projects[0].Cost.Partial = true
	s.Models[0].Cost.Partial = true
	for _, page := range []statsPage{pageOverview, pageProjects, pageModels} {
		out := strings.Join(pageLines(page, s, 100, false, false), "\n")
		if !strings.Contains(out, "$1,862+") && page != pageModels {
			t.Errorf("%s: the first project's partial spend has no +:\n%s", page, out)
		}
		if !strings.Contains(out, "$3,270+") && page != pageProjects {
			t.Errorf("%s: the first model's partial spend has no +:\n%s", page, out)
		}
	}
}

// Stacked lists line up their numbers as well as their bars, even when one
// list's numbers are wider ("unpriced").
func TestStatsStackedListsAlignTheirNumbers(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	s.Models[2].Priced, s.Models[2].Cost = false, stats.Cost{}
	s.Models[2].Label = "claude-haiku-5"
	for _, width := range []int{60, 70, 79} {
		lines := pageLines(pageOverview, s, width, false, false)
		var ends []int
		for _, line := range lines {
			for _, marker := range []string{"$1,862", "$3,270", "unpriced"} {
				if strings.HasSuffix(line, marker) {
					ends = append(ends, visibleWidth(line))
				}
			}
		}
		if len(ends) != 3 || ends[0] != ends[1] || ends[1] != ends[2] {
			t.Errorf("width %d: the numbers end at columns %v:\n%s", width, ends, strings.Join(lines, "\n"))
		}
	}
}

// The guide's screens are the screens the program prints: every block of text
// in docs/guides/stats.md that starts like a screen is one of the golden
// screens, character for character.
func TestStatsGuideShowsTheGoldenScreens(t *testing.T) {
	t.Parallel()
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "guides", "stats.md"))
	if err != nil {
		t.Fatal(err)
	}
	goldens, err := filepath.Glob(filepath.Join("testdata", "stats", "pages", "*.golden"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	screens := map[string]bool{}
	for _, path := range goldens {
		if strings.Contains(path, ".color.") || strings.Contains(path, ".ascii.") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		screens[strings.TrimSpace(string(data))] = true
	}
	blocks := regexp.MustCompile("(?s)```text\n(.*?)```").FindAllStringSubmatch(string(guide), -1)
	shown := 0
	for _, block := range blocks {
		text := strings.TrimSpace(block[1])
		if !strings.HasPrefix(text, "agent-archive stats") {
			continue
		}
		shown++
		if !screens[text] {
			t.Errorf("a screen in the guide is not one of the golden screens (regenerate it from testdata/stats/pages):\n%s", text)
		}
	}
	if shown < 5 {
		t.Errorf("the guide shows %d screens, want the summary, detail, projects, models and agents", shown)
	}
}

// A share that is not between 0 and 1 (a bug upstream, not something an
// archive can produce) never panics the screen or widens a bar.
func TestStatsStackedBarSurvivesAnyShare(t *testing.T) {
	t.Parallel()
	p := &statsPrinter{g: unicodeGlyphs}
	for _, shares := range [][]float64{
		{math.Inf(1), 0.5}, {math.NaN(), 0.5}, {1e300, 1e300}, {-1, 2}, {math.Inf(-1), math.Inf(1), math.NaN()}, {math.MaxFloat64},
	} {
		segments := make([]segment, len(shares))
		for i, s := range shares {
			segments[i] = segment{share: s}
		}
		for _, width := range []int{1, 20, 47, 60} {
			bar := p.stackedBar(segments, width)
			if got := visibleWidth(bar); got > max(width, len(shares)*2) {
				t.Errorf("%v in %d: %d wide", shares, width, got)
			}
		}
	}
}

// A window of one session has no "costliest session" to look at.
func TestStatsHeadsUpSkipsTheCostliestOfOneSession(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	if out := strings.Join(pageLines(pageOverview, s, 100, false, false), "\n"); !strings.Contains(out, "Costliest session") {
		t.Fatalf("no costliest session in a window of many:\n%s", out)
	}
	s.Coverage.Sessions = 1
	if out := strings.Join(pageLines(pageOverview, s, 100, false, false), "\n"); strings.Contains(out, "Costliest session") {
		t.Errorf("a costliest session in a window of one:\n%s", out)
	}
}
