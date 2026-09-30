package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// positions are where each name first appears in text, in order; a name that
// is missing is -1.
func positions(text string, names []string) []int {
	out := make([]int, len(names))
	for i, name := range names {
		out[i] = strings.Index(text, name)
	}
	return out
}

// checkNamesInOrder fails unless every name is in text, first to last.
func checkNamesInOrder(tb testing.TB, where, text string, names []string) {
	tb.Helper()
	at := positions(text, names)
	for i, p := range at {
		if p < 0 || (i > 0 && p < at[i-1]) {
			tb.Errorf("%s: %v are at %v, want every one, in that order:\n%s", where, names, at, text)
			return
		}
	}
}

// One archive, every way to read it: the projects are in the same order in
// --json, in --by project, on the projects screen, in the overview's top few
// and on the page, and every list cuts after the same project, whatever the
// tokens are (five projects have far more tokens, all of them cheap cache
// reads, than the two that cost the most).
func TestStatsProjectOrderIsTheSameEverywhere(t *testing.T) {
	t.Parallel()
	var build func(testing.TB, *storagetest.MemoryStore)
	for _, sc := range crossScenarios() {
		if sc.name == "projects-ranked-by-spend-not-tokens" {
			build = sc.build
		}
	}
	if build == nil {
		t.Fatal("the scenario is missing")
	}
	env, mem := statsEnv(t)
	build(t, mem)
	base := []string{"--prices", goldenPrices}
	want := []string{
		"output-heavy-1", "output-heavy-0", "cache-heavy-4", "cache-heavy-3", "cache-heavy-2", "cache-heavy-1", "cache-heavy-0",
		"unpriced-project", "cursor-only",
	}

	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, append([]string{"--json", "--by", "project"}, base...)...)), &doc); err != nil {
		t.Fatal(err)
	}
	var top []string
	for _, p := range doc.Projects {
		top = append(top, p.Name)
	}
	if !slices.Equal(top, want[:5]) || doc.TotalProjects != len(want) {
		t.Fatalf("--json projects %v of %d, want %v of %d", top, doc.TotalProjects, want[:5], len(want))
	}
	var grouped []string
	for _, row := range doc.Groups.Rows {
		grouped = append(grouped, row.Key)
	}
	if !slices.Equal(grouped, want) {
		t.Errorf("--json --by project rows %v, want %v", grouped, want)
	}

	// The projects screen lists every one.
	checkNamesInOrder(t, "the projects screen", mustRunStats(t, env, 0, append([]string{"--view", "projects"}, base...)...), want)
	// The table of --by project too.
	checkNamesInOrder(t, "--by project", mustRunStats(t, env, 0, append([]string{"--by", "project"}, base...)...), want)

	// The overview lists the top four and says how many more there are.
	overview := mustRunStats(t, env, 80, base...)
	_, section, found := strings.Cut(overview, "By project")
	if !found {
		t.Fatalf("the overview has no by-project list:\n%s", overview)
	}
	checkNamesInOrder(t, "the overview", section, want[:4])
	if strings.Contains(section, want[4]) || !strings.Contains(section, "+ 5 more") {
		t.Errorf("the overview should stop after %q and say 5 more:\n%s", want[3], section)
	}

	// The page keeps the document's top five, in its order.
	page := mustRunStats(t, env, 0, append([]string{"--html", "--include-names"}, base...)...)
	_, afterHeading, found := strings.Cut(page, `id="h-projects"`)
	if !found {
		t.Fatal("the page has no by-project table")
	}
	table, _, _ := strings.Cut(afterHeading, `id="h-models"`)
	checkNamesInOrder(t, "the page", table, want[:5])
	if strings.Contains(table, want[5]) || !strings.Contains(table, "and 4 more projects, not shown") {
		t.Errorf("the page should stop after %q and say 4 more:\n%s", want[4], table)
	}
}
