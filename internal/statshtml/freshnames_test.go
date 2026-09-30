package statshtml

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// freshNames are the names of a client's work in freshSessions. None of them
// may be on a shareable page, in any position and in any letter case.
var freshNames = []string{
	"acme", "northwind", "zephyr", "b7xk", "ft:gpt", "secret-skill", "-crm", "-jira", "billing", "internal", "prod-deploy",
	"witchcraft",
}

// freshSessions is an archive of real-looking names: a cache-heavy project
// with the most tokens, a group of output-heavy projects that cost more per
// token (the costliest session is in one of them, and some are outside the
// engine's top few projects by tokens), a fine-tune and a deployment name as
// models, a plugin's skill, MCP servers, subagents, and Cursor sessions
// without tokens.
func freshSessions() []archive.Metadata {
	fineTune, deployment := "ft:gpt-4.1:acme-corp::B7xk", "acme-prod-deploy"
	var specs []sessionSpec
	for i := range 20 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("a-%02d", i), harness: "claude", project: "acme-billing", captured: day(time.September, 10+i%15, 9),
			models: []string{"claude-opus-5"}, turns: 9, messages: 80, toolResults: 100, errors: 3,
			tokens: []tokenSpec{{"claude-opus-5", 100_000, 50_000, 30_000_000, 1_000_000}},
			skills: []string{"acme-internal-deploy"}, mcp: map[string]int{"acme-jira": 5},
		})
	}
	for i := range 6 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("n-%02d", i), harness: "claude", project: fmt.Sprintf("client-northwind-%d", i), captured: day(time.September, 12+i, 9),
			models: []string{"claude-fable-5"}, turns: 5, messages: 30, toolResults: 20, errors: 1,
			tokens: []tokenSpec{{"claude-fable-5", 20_000, 900_000 + i*300_000, 200_000, 10_000}},
			skills: []string{"acme-plugin:secret-skill", "anthropic-skills:docs"}, mcp: map[string]int{"northwind-crm": 2},
		})
	}
	specs = append(specs, sessionSpec{
		id: "ft-1", harness: "codex", project: "zephyr-ft", captured: day(time.September, 20, 9),
		models: []string{fineTune, deployment}, turns: 5, messages: 30, toolResults: 20,
		tokens: []tokenSpec{{fineTune, 10_000, 5_000, 200_000, 0}, {deployment, 30_000, 5_000, 2_000_000, 0}},
	})
	for i := range 4 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("cur-%d", i), harness: "cursor", project: "cursor-witchcraft", captured: day(time.September, 25, 9+i),
			models: []string{"cursor-auto"}, turns: 3,
		})
	}
	for i := range 8 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("s-%d", i), harness: "claude", project: fmt.Sprintf("acme-small-%d", i), captured: day(time.September, 28, 9),
			models: []string{"claude-haiku-4-5"}, turns: 1, messages: 3, toolResults: 1,
			tokens: []tokenSpec{{"claude-haiku-4-5", 1_000, 100, 0, 0}},
		})
	}
	specs = append(specs, sessionSpec{
		id: "n-sub", harness: "claude", project: "client-northwind-5", captured: day(time.September, 17, 9), parent: "n-05",
		models: []string{"claude-fable-5"}, turns: 1, tokens: []tokenSpec{{"claude-fable-5", 1_000, 300_000, 10_000, 0}},
	})
	out := make([]archive.Metadata, len(specs))
	for i, s := range specs {
		out[i] = s.build()
	}
	return out
}

// freshStats is the stats of freshSessions at the fixture's clock, priced by
// the built-in table.
func freshStats(days int, by stats.Grouping, all bool) stats.Stats {
	return stats.Compute(freshSessions(), stats.Options{
		Now: fixtureNow, Days: days, Location: time.UTC, PriceTable: stats.DefaultPriceTable(), By: by, AllRows: all,
	})
}

// A default page of an archive with real-looking names has none of them, in
// any window, with any breakdown, with or without every row, and with a filter
// that names the fine-tune: the text, titles, labels, table cells and the
// heads-up notes are all read.
func TestFreshArchiveNamesNothingOnAShareablePage(t *testing.T) {
	t.Parallel()
	for _, days := range []int{7, 30, 90} {
		for _, by := range []stats.Grouping{stats.GroupNone, stats.GroupProject, stats.GroupWeek} {
			for _, all := range []bool{false, true} {
				t.Run(fmt.Sprintf("%dd-by-%q-all-%v", days, by, all), func(t *testing.T) {
					t.Parallel()
					s := freshStats(days, by, all)
					page := strings.ToLower(string(render(t, s, Options{Filters: Filters{Harness: "claude", Model: "ft:gpt-4.1:acme-corp::B7xk"}})))
					for _, name := range freshNames {
						if i := strings.Index(page, name); i >= 0 {
							t.Errorf("%q is on the shareable page: ...%s...", name, page[max(0, i-60):min(len(page), i+60)])
						}
					}
				})
			}
		}
	}
	// The same archive with names shown does name them, so the check above can
	// fail.
	if page := strings.ToLower(string(render(t, freshStats(30, stats.GroupProject, false), Options{IncludeNames: true}))); !strings.Contains(page, "acme-billing") {
		t.Error("--include-names does not show the project")
	}
}

// Each day's bar is as tall as its spend is a share of the dearest day's, a day
// with no sessions is the axis's mark, a day with sessions and no cost is the
// grey mark of an unknown, and only the dearest day is the peak.
func TestDailyBarHeightsFollowSpend(t *testing.T) {
	t.Parallel()
	bar := regexp.MustCompile(`<rect class="bar ([a-z ]+)" x="[0-9.]+%" width="[0-9.]+%" y="[0-9.]+" height="([0-9.]+)"`)
	for name, s := range map[string]stats.Stats{
		"fixture":   computeFixture(t, fixtureSessions(), 30, stats.GroupNone),
		"realistic": modelStats(t, realisticSessions(), realisticPrices, stats.GroupNone),
		"fresh-7":   freshStats(7, stats.GroupNone, false),
		"fresh-30":  freshStats(30, stats.GroupNone, false),
		"fresh-90":  freshStats(90, stats.GroupNone, false),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bars := bar.FindAllStringSubmatch(string(render(t, s, Options{})), -1)
			if len(bars) != len(s.Daily) {
				t.Fatalf("%d bars for %d days", len(bars), len(s.Daily))
			}
			plot := float64(chartBaseline - chartPlotTop)
			for i, d := range s.Daily {
				class := bars[i][1]
				height, err := strconv.ParseFloat(bars[i][2], 64)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case d.Sessions == 0:
					if class != "zero" {
						t.Errorf("%s has no sessions but is drawn as %q", d.Date, class)
					}
				case d.Cost.USD == nil:
					if class != "unknown" {
						t.Errorf("%s has no cost but is drawn as %q", d.Date, class)
					}
				default:
					want := max(plot**d.Cost.USD/s.PeakSpend.USD, 2)
					if *d.Cost.USD == 0 {
						want = 1
					}
					if math.Abs(height-want) > 0.01 {
						t.Errorf("%s: the bar is %v tall, want %v", d.Date, height, want)
					}
					if peak := strings.Contains(class, "peak"); peak != (d.Date == s.PeakSpend.Date) {
						t.Errorf("%s: drawn as %q, the peak is %s", d.Date, class, s.PeakSpend.Date)
					}
				}
			}
		})
	}
}
