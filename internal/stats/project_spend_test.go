package stats

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Every project's spend adds up to the overall spend, and its unpriced tokens
// to the overall unpriced tokens, however the archive is laid out: subagents
// rolled up into their root's project, sessions priced at the main model (a
// sidecar from before parser 0.14.0), models with no price, sessions with no
// tokens and the project with no name. Grouping by project lists the same
// rows, in the same order, as the list of every project.
func TestProjectSpendAddsUpToTheOverallSpend(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7919))
		sessions, _ := randomArchive(rng, 40+rng.IntN(160))
		o := Options{Now: time.Date(2026, time.October, 1, 0, 0, 0, 0, newYork), Location: newYork, Days: 400, AllRows: true, By: GroupProject}
		got := Compute(sessions, o)
		var sum float64
		var unpriced int64
		var priced bool
		for _, p := range got.Projects {
			if p.Cost.USD != nil {
				sum += *p.Cost.USD
				priced = true
			}
			unpriced += p.Cost.UnpricedTokens
		}
		overall, overallPriced := 0.0, got.Overview.Cost.Value != nil
		if overallPriced {
			overall = *got.Overview.Cost.Value
		}
		if priced != overallPriced || !near2(sum, overall) {
			t.Errorf("seed %d: the projects' spend adds to %v (priced %v), the overall spend is %v (priced %v)", seed, sum, priced, overall, overallPriced)
		}
		if unpriced != got.Overview.Cost.UnpricedTokens {
			t.Errorf("seed %d: the projects' unpriced tokens add to %d, the overall %d", seed, unpriced, got.Overview.Cost.UnpricedTokens)
		}
		if len(got.Groups.Rows) != len(got.Projects) {
			t.Fatalf("seed %d: %d rows grouped by project, %d projects", seed, len(got.Groups.Rows), len(got.Projects))
		}
		for i, row := range got.Groups.Rows {
			p := got.Projects[i]
			if row.Key != p.Name || row.Sessions != p.Sessions || mustJSON(t, row.Tokens) != mustJSON(t, p.Tokens) || mustJSON(t, row.Cost) != mustJSON(t, p.Cost) {
				t.Fatalf("seed %d row %d: grouped %+v, listed %+v", seed, i, row, p)
			}
		}
	}
}

// A subagent's spend is its root session's project's spend, whatever project
// the subagent's own record names, so the ranking sees the whole cost of a
// session; the project of an orphan subagent (whose parent is missing) is its
// own.
func TestProjectSpendIncludesRolledUpSubagents(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	sessions := []archive.Metadata{
		meta("root", "claude", at, project("small-root"), modelTokens("claude-opus-5-5", 0, 100_000, 0, 0)),
		// Two subagents that outspend their root, one recording another project.
		meta("sub-1", "claude", at, project("small-root"), parentOf("root"), modelTokens("claude-opus-5-5", 0, 2_000_000, 0, 0)),
		meta("sub-2", "claude", at, project("elsewhere"), parentOf("root"), modelTokens("claude-opus-5-5", 0, 2_000_000, 0, 0)),
		meta("mid", "claude", at, project("mid"), modelTokens("claude-opus-5-5", 0, 3_000_000, 0, 0)),
		meta("orphan", "claude", at, project("orphans"), parentOf("nowhere"), modelTokens("claude-opus-5-5", 0, 1_000_000, 0, 0)),
	}
	got := Compute(sessions, Options{Now: now, Location: newYork, AllRows: true})
	if names := projectNames(got.Projects); !slices.Equal(names, []string{"small-root", "mid", "orphans"}) {
		t.Fatalf("projects %v, want the root's with its subagents' spend first", names)
	}
	root, mid, orphan := got.Projects[0], got.Projects[1], got.Projects[2]
	perMillion := *orphan.Cost.USD
	if !near2(*root.Cost.USD, 4.1*perMillion) || !near2(*mid.Cost.USD, 3*perMillion) || root.Sessions != 1 {
		t.Errorf("spend: root %v, mid %v, orphan %v (sessions %d)", *root.Cost.USD, *mid.Cost.USD, perMillion, root.Sessions)
	}
	if sum := *root.Cost.USD + *mid.Cost.USD + *orphan.Cost.USD; !near2(sum, *got.Overview.Cost.Value) {
		t.Errorf("the projects' spend %v is not the overall %v", sum, *got.Overview.Cost.Value)
	}
}

// Ranking a partly priced project on the spend it has can leave a project
// whose real cost is dearer below a cheaper one, and a project with no priced
// cost at all is after every priced one (and cut first), however many tokens
// it has. The list does not say so by itself: the overall spend is partial, its
// unpriced tokens are counted, and the models list flags the model with no
// price, whatever the cut left out.
func TestAnUnpricedProjectCutFromTheListStaysVisibleInTheTotals(t *testing.T) {
	t.Parallel()
	var sessions []archive.Metadata
	at := day(time.September, 20, 10)
	for i := range DefaultTopN {
		sessions = append(sessions, meta(fmt.Sprintf("priced-%d", i), "claude", at, project(fmt.Sprintf("priced-%d", i)),
			modelTokens("claude-opus-5-5", 0, 10_000+i*1_000, 0, 0)))
	}
	sessions = append(sessions, meta("mystery", "codex", at, project("mystery"), modelTokens("no-such-model", 90_000_000, 0, 0, 0)))
	got := Compute(sessions, Options{Now: now, Location: newYork})
	if got.TotalProjects != DefaultTopN+1 || slices.Contains(projectNames(got.Projects), "mystery") {
		t.Fatalf("%d projects, kept %v: the unpriced project should be the one cut", got.TotalProjects, projectNames(got.Projects))
	}
	if cost := got.Overview.Cost; cost.Value == nil || !cost.Partial || cost.UnpricedTokens != 90_000_000 {
		t.Errorf("overall spend %+v, want a partial one with 90000000 unpriced tokens", cost)
	}
	var flagged bool
	for _, m := range got.Models {
		if !m.Priced && m.Tokens == 90_000_000 {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("no unpriced model in %+v carrying the cut project's tokens", got.Models)
	}
}
