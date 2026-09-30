package stats

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// randomArchive builds a varied archive: three agents, priced and unpriced
// models, per-model and old-style sidecars, sessions with no tokens at all,
// subagent chains (some orphaned), and captures spread over ~200 days. It
// also returns the oracle: the tokens every session holds, normalized to
// fresh + cache read + cache write + output, counted straight from the
// metadata with no grouping.
func randomArchive(rng *rand.Rand, n int) (sessions []archive.Metadata, oracleTokens int64) {
	harnesses := []string{"claude", "codex", "cursor"}
	models := map[string][]string{
		"claude": {"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-4-5-20251001", "claude-opus-5-5[1m]", "opus", "unknown", ""},
		"codex":  {"gpt-5.6-terra", "gpt-6-luna", "codex-auto-review", "unknown"},
		"cursor": {"composer-1"},
	}
	projects := []string{"alpha", "beta", "gamma", "", "delta"}
	epoch := time.Date(2026, time.March, 1, 0, 0, 0, 0, newYork)
	for i := range n {
		harness := harnesses[rng.IntN(len(harnesses))]
		at := epoch.Add(time.Duration(rng.IntN(200*24*60)) * time.Minute)
		m := meta(fmt.Sprintf("s%03d", i), harness, at, project(projects[rng.IntN(len(projects))]))
		if rng.IntN(3) > 0 {
			m.Counts.Turns = ip(rng.IntN(30))
		}
		if i > 5 && rng.IntN(4) == 0 {
			// A subagent of an earlier session (sometimes a missing one).
			parent := fmt.Sprintf("s%03d", rng.IntN(i))
			if rng.IntN(8) == 0 {
				parent = "missing"
			}
			m.ParentSessionID = parent
			m.Harness.Name = "claude"
			harness = "claude"
		}
		switch shape := rng.IntN(5); {
		case shape == 0 || harness == "cursor":
			// No token data at all.
		case shape == 1:
			// A sidecar from before parser 0.14.0: session-wide counts, models[] only.
			m.Parser.Version = "0.13.0"
			id := models[harness][rng.IntN(len(models[harness]))]
			usedModel(id, 1+rng.IntN(5))(&m)
			input, out, read, write := rng.IntN(5000), rng.IntN(1000), rng.IntN(5000), rng.IntN(500)
			if harness == "codex" {
				read = min(read, input)
				write = 0
			}
			tokens(input, out, read, write)(&m)
		default:
			for range 1 + rng.IntN(3) {
				id := models[harness][rng.IntN(len(models[harness]))]
				input, out, read, write := rng.IntN(5000), rng.IntN(1000), rng.IntN(5000), rng.IntN(500)
				if harness == "codex" {
					read = min(read, input)
					write = 0
				}
				modelTokens(id, input, out, read, write)(&m)
			}
		}
		if m.Counts.InputTokens != nil {
			oracleTokens += int64(*m.Counts.OutputTokens + *m.Counts.CacheReadTokens + *m.Counts.CacheWriteTokens)
			input := *m.Counts.InputTokens
			if harness == "codex" {
				input -= *m.Counts.CacheReadTokens
			}
			oracleTokens += int64(input)
		}
		sessions = append(sessions, m)
	}
	return sessions, oracleTokens
}

func sumTokens(vals ...*int64) int64 {
	var sum int64
	for _, v := range vals {
		if v != nil {
			sum += *v
		}
	}
	return sum
}

// Whatever the archive holds, the per-agent, per-model, per-project, per-day
// and per-group token totals each add up to the overall total, and that
// total is what the raw metadata holds.
func TestBreakdownsAddUpToTheTotal(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7919))
		sessions, oracle := randomArchive(rng, 40+rng.IntN(160))
		// A window wide enough to hold every session, whatever its date.
		wide := Options{Now: time.Date(2026, time.October, 1, 0, 0, 0, 0, newYork), Location: newYork, Days: 400, TopN: 10_000}
		for _, by := range []Grouping{GroupNone, GroupDay, GroupWeek, GroupMonth, GroupProject} {
			wide.By = by
			got := Compute(sessions, wide)
			check := func(name string, have, want int64) {
				if have != want {
					t.Errorf("seed %d by %q: %s = %d, want %d", seed, by, name, have, want)
				}
			}
			var total int64
			if got.Overview.Tokens.Value != nil {
				total = int64(*got.Overview.Tokens.Value)
			}
			check("overall tokens vs the raw metadata", total, oracle)

			var agentsSum, modelsSum, projectsSum, dailySum, groupSum, sessionsSum, agentSessions int64
			for _, a := range got.Agents {
				agentsSum += sumTokens(a.Tokens)
				agentSessions += int64(a.Sessions)
			}
			for _, r := range got.Models {
				modelsSum += r.Tokens
			}
			for _, p := range got.Projects {
				projectsSum += sumTokens(p.Tokens)
				sessionsSum += int64(p.Sessions)
			}
			for _, d := range got.Daily {
				dailySum += sumTokens(d.Tokens)
			}
			check("per-agent tokens", agentsSum, total)
			check("per-model tokens", modelsSum, total)
			check("per-project tokens", projectsSum, total)
			check("per-day tokens", dailySum, total)
			check("per-agent sessions", agentSessions, int64(got.Coverage.Sessions))
			check("per-project sessions", sessionsSum, int64(got.Coverage.Sessions))
			if got.Composition != nil {
				c := got.Composition
				check("composition segments", c.CacheRead.Tokens+c.CacheWrite.Tokens+c.FreshInput.Tokens+c.Output.Tokens, total)
				check("composition total", c.Total, total)
			} else {
				check("tokens without a composition", total, 0)
			}
			if got.Groups != nil {
				var groupSessions int64
				for _, row := range got.Groups.Rows {
					groupSum += sumTokens(row.Tokens)
					groupSessions += int64(row.Sessions)
				}
				check("per-group tokens", groupSum, total)
				check("per-group sessions", groupSessions, int64(got.Coverage.Sessions))
			}

			// Cost adds up too: priced plus unpriced tokens are all the tokens, and
			// each breakdown's dollars add to the overall.
			var agentCost, modelCost, projectCost float64
			var agentUnpriced, modelUnpriced int64
			for _, a := range got.Agents {
				if a.Cost.USD != nil {
					agentCost += *a.Cost.USD
				}
				agentUnpriced += a.Cost.UnpricedTokens
			}
			for _, r := range got.Models {
				if r.Cost.USD != nil {
					modelCost += *r.Cost.USD
				}
				modelUnpriced += r.Cost.UnpricedTokens
			}
			for _, p := range got.Projects {
				if p.Cost.USD != nil {
					projectCost += *p.Cost.USD
				}
			}
			overall := 0.0
			if got.Overview.Cost.Value != nil {
				overall = *got.Overview.Cost.Value
			}
			for name, sum := range map[string]float64{"agents": agentCost, "models": modelCost, "projects": projectCost} {
				if !near2(sum, overall) {
					t.Errorf("seed %d: %s cost %v != overall %v", seed, name, sum, overall)
				}
			}
			check("per-agent unpriced tokens", agentUnpriced, got.Overview.Cost.UnpricedTokens)
			check("per-model unpriced tokens", modelUnpriced, got.Overview.Cost.UnpricedTokens)
		}
	}
}

func near2(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9*(1+abs(a)+abs(b))
}

func abs(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}

// The same archive in any order, and computed twice, gives the same document.
func TestComputeIsDeterministicWhateverTheInputOrder(t *testing.T) {
	t.Parallel()
	for seed := uint64(100); seed < 110; seed++ {
		rng := rand.New(rand.NewPCG(seed, 1))
		sessions, _ := randomArchive(rng, 120)
		o := Options{Now: time.Date(2026, time.September, 20, 12, 0, 0, 0, newYork), Location: newYork, Days: 60, By: GroupWeek}
		want := mustJSON(t, Compute(sessions, o))
		if again := mustJSON(t, Compute(sessions, o)); again != want {
			t.Fatalf("seed %d: two runs differ", seed)
		}
		shuffled := append([]archive.Metadata(nil), sessions...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := mustJSON(t, Compute(shuffled, o)); got != want {
			t.Fatalf("seed %d: input order changed the result", seed)
		}
	}
}

// Compute must not change what it is given.
func TestComputeDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()
	sessions := multiAgentSessions()
	before := mustJSON(t, sessions)
	Compute(sessions, opts())
	if after := mustJSON(t, sessions); after != before {
		t.Fatal("Compute changed its input")
	}
}
