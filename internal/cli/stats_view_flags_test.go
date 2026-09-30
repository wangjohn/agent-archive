package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --view chooses a screen, --detail is --view detail, and --by project is the
// projects screen; anything that would need a guess is a usage error, checked
// before the archive is read: exit 2, one line on stderr, nothing on stdout.
func TestStatsViewFlagMatrix(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--view", "summary"}, `--view must be overview, detail, projects, models, or agents, not "summary"`},
		{[]string{"--view", ""}, `--view must be overview, detail, projects, models, or agents, not ""`},
		{[]string{"--view", "Detail"}, "--view must be"},
		{[]string{"--view", "detail", "--detail"}, "choose one of --view and --detail"},
		{[]string{"--detail", "--view", "overview"}, "choose one of --view and --detail"},
		{[]string{"--view", "projects", "--by", "day"}, "--by day is shown on the detail screen; drop --by or use --view detail"},
		{[]string{"--view", "overview", "--by", "project"}, "--by project is shown on the projects screen; drop --by or use --view projects"},
		{[]string{"--view", "models", "--by", "week"}, "--by week is shown on the detail screen"},
		{[]string{"--detail", "--by", "project"}, "--by project is shown on the projects screen"},
		{[]string{"--json", "--view", "detail"}, "--view and --detail choose a screen; --json and --html print the whole document"},
		{[]string{"--json", "--detail"}, "--view and --detail choose a screen"},
		{[]string{"--html", "--view", "projects"}, "--view and --detail choose a screen"},
		{[]string{"--html", "--detail"}, "--view and --detail choose a screen"},
	} {
		out, errOut, code := runStats(t, env, 0, tc.args...)
		if code != 2 || out != "" || !strings.Contains(errOut, tc.want) || !strings.Contains(errOut, "run agent-archive stats --help") || strings.Count(errOut, "\n") != 1 {
			t.Errorf("%v: code=%d stdout=%q stderr=%q, want exit 2 mentioning %q", tc.args, code, out, errOut, tc.want)
		}
	}
}

// Each screen is what --view names, --detail is --view detail, and --by
// chooses the screen it belongs on when --view does not.
func TestStatsViewFlagsChooseTheScreen(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	screen := func(args ...string) string {
		return mustRunStats(t, env, 100, append([]string{"--prices", goldenPrices}, args...)...)
	}
	marks := map[string]string{
		"overview": "WHERE IT WENT", "detail": "WHAT USED YOUR TOKENS", "projects": "PROJECTS (3)", "models": "MODELS (4)", "agents": "cache hit",
	}
	for view, mark := range marks {
		out := screen("--view", view)
		if !strings.Contains(out, mark) {
			t.Errorf("--view %s lacks %q:\n%s", view, mark, out)
		}
		for other, otherMark := range marks {
			// The detail screen has an AGENTS table with the cache hit column
			// the agents screen has too.
			if other != view && strings.Contains(out, otherMark) && (view != "detail" || other != "agents") {
				t.Errorf("--view %s has %q, which belongs to %s:\n%s", view, otherMark, other, out)
			}
		}
	}
	if screen() != screen("--view", "overview") {
		t.Error("the default screen is not the overview")
	}
	if screen("--detail") != screen("--view", "detail") {
		t.Error("--detail is not --view detail")
	}
	if screen("--by", "project") != screen("--view", "projects") || screen("--by", "project", "--view", "projects") != screen("--view", "projects") {
		t.Error("--by project is not the projects screen")
	}
	for _, by := range []string{"day", "week", "month"} {
		out := screen("--by", by)
		if !strings.Contains(out, "BY "+strings.ToUpper(by)) || !strings.Contains(out, "WHAT USED YOUR TOKENS") {
			t.Errorf("--by %s is not a table under the detail screen:\n%s", by, out)
		}
		if out != screen("--by", by, "--view", "detail") || out != screen("--by", by, "--detail") {
			t.Errorf("--by %s differs with --view detail or --detail", by)
		}
	}
}

// --json and --html are unaffected by the screens: the JSON keeps the engine's
// default lists (the top five projects, with the total), and the same
// document comes whatever the terminal is.
func TestStatsJSONAndHTMLAreUnaffectedByTheScreens(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	for i := range 8 {
		syntheticSession{
			id: fmt.Sprintf("p%d", i), harness: "claude", project: fmt.Sprintf("project-%d", i), captured: statsDay(time.September, 20+i, 9),
			models: []string{"claude-opus-5"}, turns: 2, skills: []string{fmt.Sprintf("skill-%d", i), "plugin:common"}, mcp: map[string]int{fmt.Sprintf("srv-%d", i): 2},
			perModel: []modelTokenSpec{{"claude-opus-5", 1000 * (i + 1), 500, 200, 100}},
		}.publish(t, mem)
	}
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--json")), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Projects) != 5 || doc.TotalProjects != 8 || len(doc.Skills) != 5 || doc.TotalSkills != 9 {
		t.Fatalf("--json lists %d projects of %d, %d skills of %d; want the top 5 of each", len(doc.Projects), doc.TotalProjects, len(doc.Skills), doc.TotalSkills)
	}
	for _, width := range []int{0, 60, 120} {
		if got := mustRunStats(t, env, width, "--json"); got != mustRunStats(t, env, 0, "--json") {
			t.Errorf("--json differs on a terminal of %d columns", width)
		}
	}
	page := mustRunStats(t, env, 0, "--html")
	if plain := mustRunStats(t, env, 200, "--html"); plain != page {
		t.Error("--html differs with the terminal")
	}
	// The screens list what the JSON leaves at the top five.
	overview := mustRunStats(t, env, 100)
	if !strings.Contains(overview, "+ 4 more") {
		t.Errorf("the overview does not say how many projects it leaves out:\n%s", overview)
	}
	projects := mustRunStats(t, env, 100, "--view", "projects")
	for i := range 8 {
		if !strings.Contains(projects, fmt.Sprintf("project-%d", i)) {
			t.Errorf("the projects screen lacks project-%d:\n%s", i, projects)
		}
	}
	detail := mustRunStats(t, env, 100, "--detail")
	for i := range 8 {
		if !strings.Contains(detail, fmt.Sprintf("skill-%d", i)) || !strings.Contains(detail, fmt.Sprintf("srv-%d", i)) {
			t.Errorf("the detail screen lacks skill-%d or srv-%d:\n%s", i, i, detail)
		}
	}
	// A plugin's prefix is dropped for display only.
	if !strings.Contains(detail, "common 8") || strings.Contains(detail, "plugin:") {
		t.Errorf("the skill's plugin prefix is not stripped on the screen:\n%s", detail)
	}
	if !strings.Contains(mustRunStats(t, env, 0, "--json"), "plugin:common") {
		t.Error("the JSON lost the skill's full name")
	}
}

// Every screen prints without a pager or a terminal, for every filter.
func TestStatsEveryScreenPrintsWithEveryFilter(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	for _, view := range []string{"overview", "detail", "projects", "models", "agents"} {
		for _, extra := range [][]string{nil, {"--harness", "cursor"}, {"--harness", "codex"}, {"--days", "1"}, {"--days", "400"}, {"--model", "gpt-5"}} {
			for _, width := range []int{0, 40, 60, 80, 250} {
				out, errOut, code := runStats(t, env, width, append([]string{"--view", view}, extra...)...)
				if code != 0 || errOut != "" || strings.TrimSpace(out) == "" {
					t.Fatalf("--view %s %v at %d: code=%d stderr=%q stdout=%q", view, extra, width, code, errOut, out)
				}
			}
		}
	}
}
