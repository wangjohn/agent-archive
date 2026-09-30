package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

// The terminal screens are drawn from the same numbers as `--json`. These
// tests run both over the same archives (the scenarios of the web page's
// cross-check) and compare what the screens say with what the document holds,
// so a screen cannot show a number the document does not have.

// withoutBy is args without --by and its value: a screen is chosen by --view,
// and the document's grouping does not change what the screens list.
func withoutBy(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--by" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// lineStarting is the first line of screen after its title that starts with
// prefix.
func lineStarting(screen, prefix string) string {
	_, rest, _ := strings.Cut(screen, "\n")
	for line := range strings.SplitSeq(rest, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func TestStatsScreensAgreeWithJSON(t *testing.T) {
	t.Parallel()
	for _, sc := range crossScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			env, mem := statsEnv(t)
			sc.build(t, mem)
			base := append([]string{"--prices", goldenPrices}, withoutBy(sc.args)...)
			var doc statsDocument
			if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, append([]string{"--json"}, base...)...)), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Coverage.Sessions == 0 {
				return
			}
			screen := func(view string) string {
				return mustRunStats(t, env, 120, append([]string{"--view", view}, base...)...)
			}
			overview, detail := screen("overview"), screen("detail")
			o := doc.Overview
			mustSay := func(where, screen string, wants ...string) {
				t.Helper()
				flat := flatten(screen)
				for _, want := range wants {
					if !strings.Contains(flat, want) {
						t.Errorf("%s does not say %q:\n%s", where, want, screen)
					}
				}
			}
			mustSay("the overview", overview,
				count(int64(math.Round(*o.Sessions.Value)), "session"))
			if o.Tokens.Value != nil {
				mustSay("the overview", overview, statsfmt.TokenCount(int64(math.Round(*o.Tokens.Value)))+" tokens")
			} else {
				mustSay("the overview", overview, "tokens unknown")
			}
			if o.Cost.Value != nil {
				mustSay("the overview", overview, "~"+statsfmt.Money(doc.Prices.Currency, *o.Cost.Value, false))
			} else {
				mustSay("the overview", overview, "spend unknown")
			}
			if o.CacheShare != nil {
				mustSay("the overview", overview, statsfmt.Percent(*o.CacheShare)+" served from cache")
			}
			if doc.PeakSpend != nil {
				mustSay("the overview", overview, "peak ~"+statsfmt.Money(doc.Prices.Currency, doc.PeakSpend.USD, false))
			}
			for _, a := range doc.Agents {
				mustSay("the overview", overview, clean(a.Label)+" "+statsfmt.Percent(a.SessionShare))
			}
			for _, n := range doc.HeadsUp {
				if n.Kind == stats.NoteSubagentShare {
					mustSay("the overview", overview, statsfmt.Percent(*n.Share)+" of tokens came from subagents ("+plural(*n.Runs, "run")+")")
				}
			}
			// Every headline number is on the detail screen too, with the
			// previous period's.
			mustSay("the detail screen", detail, "Sessions", fmt.Sprintf("%d of %d", int(math.Round(*o.ActiveDays.Value)), o.DaysInWindow))
			if c := doc.Composition; c != nil && c.Total > 0 {
				mustSay("the detail screen", detail,
					"Cache read "+statsfmt.Percent(c.CacheRead.Share)+" "+statsfmt.TokenCount(c.CacheRead.Tokens),
					"Output "+statsfmt.Percent(c.Output.Share)+" "+statsfmt.TokenCount(c.Output.Tokens))
			}
			for _, a := range doc.Agents {
				row := lineStarting(detail, "● "+clean(a.Label))
				if row == "" {
					t.Errorf("the detail screen has no row for %s:\n%s", a.Label, detail)
					continue
				}
				for _, want := range []string{statsfmt.CommaInt(int64(a.Sessions)), statsfmt.Percent(a.SessionShare), tokensText(a.Tokens)} {
					if !strings.Contains(row, want) {
						t.Errorf("%s row %q lacks %q", a.Label, row, want)
					}
				}
			}
			checkListScreens(t, screen, doc)
		})
	}
}

func checkListScreens(t *testing.T, screen func(string) string, doc statsDocument) {
	t.Helper()
	projects, models, agents := screen("projects"), screen("models"), screen("agents")
	for _, p := range doc.Projects {
		row := lineStarting(projects, projectLabel(p.Name))
		if row == "" {
			t.Errorf("the projects screen has no row for %q:\n%s", p.Name, projects)
			continue
		}
		for _, want := range []string{statsfmt.CommaInt(int64(p.Sessions)), tokensText(p.Tokens)} {
			if !strings.Contains(row, want) {
				t.Errorf("project row %q lacks %q", row, want)
			}
		}
		if p.Cost.USD != nil && !strings.Contains(row, statsfmt.Money(doc.Prices.Currency, *p.Cost.USD, false)) {
			t.Errorf("project row %q lacks its spend %v", row, *p.Cost.USD)
		}
	}
	for _, m := range doc.Models {
		row := lineStarting(models, clean(m.Label))
		if row == "" {
			t.Errorf("the models screen has no row for %q:\n%s", m.Label, models)
			continue
		}
		want := "unpriced"
		if m.Priced && m.Cost.USD != nil {
			want = statsfmt.Money(doc.Prices.Currency, *m.Cost.USD, false)
		}
		if !strings.Contains(row, want) || !strings.Contains(row, statsfmt.TokenCount(m.Tokens)) {
			t.Errorf("model row %q lacks %q or its tokens", row, want)
		}
	}
	if len(doc.Models) == 0 && !strings.Contains(models, "No session in this window reports tokens by model") {
		t.Errorf("no models, and the screen does not say so:\n%s", models)
	}
	for _, a := range doc.Agents {
		row := lineStarting(agents, "● "+clean(a.Label))
		if row == "" || !strings.Contains(row, statsfmt.CommaInt(int64(a.Sessions))) || !strings.Contains(row, tokensText(a.Tokens)) {
			t.Errorf("the agents screen's row for %s is %q", a.Label, row)
		}
	}
}
