package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// allInHint matches the line that says how many rows a list leaves out and
// where they all are.
var allInHint = regexp.MustCompile(`(?m)^(?:\+ \d+ more|\d+ earlier rows not shown) \(all in ([^)]+)\)$`)

// hintCommand returns the command a page's hint points at: "+ 5 more (all in
// --json --by project)" names "--json --by project". A page with no such line
// fails the test.
func hintCommand(t *testing.T, page string) []string {
	t.Helper()
	m := allInHint.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page has no hint to a command that lists the rest:\n%s", page)
	}
	return strings.Fields(m[1])
}

// runHint runs the command a hint names, and decodes its JSON.
func runHint(t *testing.T, env Env, hint []string, extra ...string) statsDocument {
	t.Helper()
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, append(append([]string{"--no-cache"}, extra...), hint...)...)), &doc); err != nil {
		t.Fatalf("stats %v: %v", hint, err)
	}
	return doc
}

// Every "all in ..." hint under a list a screen cuts is true: the command it
// names really lists every row, and plain --json, which the hint used to name,
// does not (it keeps the top five projects). The archive has more projects
// and models than a list screen keeps (500).
func TestStatsAllInHintsListEveryRow(t *testing.T) {
	t.Parallel()
	const projects = statsMaxListRows + 5
	env, mem := statsEnv(t)
	for i := range projects {
		syntheticSession{
			id: fmt.Sprintf("h%03d", i), harness: "claude", project: fmt.Sprintf("project-%03d", i),
			captured: statsNow.Add(-time.Hour), models: []string{fmt.Sprintf("model-%03d", i)}, turns: 1,
			perModel: []modelTokenSpec{{fmt.Sprintf("model-%03d", i), 1000 + i, 100, 0, 0}},
		}.publish(t, mem)
	}
	// Plain --json keeps the top five projects and has no groups; that is why
	// no hint may name it for the projects.
	plain := runHint(t, env, []string{"--json"})
	if len(plain.Projects) != 5 || plain.TotalProjects != projects || plain.Groups != nil {
		t.Fatalf("plain --json has %d projects of %d, groups %v; want the top 5 of %d and no groups", len(plain.Projects), plain.TotalProjects, plain.Groups, projects)
	}

	t.Run("projects screen", func(t *testing.T) {
		t.Parallel()
		page := mustRunStats(t, env, 100, "--no-cache", "--view", "projects")
		hint := hintCommand(t, page)
		if !strings.Contains(page, "+ 5 more (all in --json --by project)") {
			t.Errorf("the projects screen says something else about the 5 it leaves out:\n%s", page[len(page)-400:])
		}
		doc := runHint(t, env, hint)
		if doc.Groups == nil || doc.Groups.By != stats.GroupProject || len(doc.Groups.Rows) != projects {
			t.Fatalf("%v does not list all %d projects: %+v", hint, projects, doc.Groups)
		}
		// The screen shows the first rows of that same list, in that order.
		for i, row := range doc.Groups.Rows[:statsMaxListRows] {
			if !strings.Contains(page, row.Key) {
				t.Fatalf("row %d of %v, %q, is not on the projects screen", i, hint, row.Key)
			}
		}
		for _, row := range doc.Groups.Rows[statsMaxListRows:] {
			if strings.Contains(page, row.Key) {
				t.Errorf("%q is one of the rows the screen says it left out", row.Key)
			}
		}
	})

	t.Run("models screen", func(t *testing.T) {
		t.Parallel()
		page := mustRunStats(t, env, 100, "--no-cache", "--view", "models")
		hint := hintCommand(t, page)
		if !strings.Contains(page, "+ 5 more (all in --json)") {
			t.Errorf("the models screen says something else about the 5 it leaves out:\n%s", page[len(page)-500:])
		}
		if doc := runHint(t, env, hint); len(doc.Models) != projects {
			t.Errorf("%v lists %d models, want all %d", hint, len(doc.Models), projects)
		}
	})
}

// The day and week tables of the detail screen keep the newest 60 rows, and
// the hint under them is true: the command it names has every row.
func TestStatsAllInHintsListEveryGroupRow(t *testing.T) {
	t.Parallel()
	// One session a week for 65 weeks: more week rows than the table keeps,
	// and more day rows too.
	const weeks = statsMaxGroupRows + 5
	env, mem := statsEnv(t)
	for i := range weeks {
		syntheticSession{
			id: fmt.Sprintf("g%03d", i), harness: "claude", project: "one", captured: statsNow.AddDate(0, 0, -7*i).Add(-time.Hour),
			models: []string{"claude-opus-5"}, turns: 1, perModel: []modelTokenSpec{{"claude-opus-5", 1000, 100, 0, 0}},
		}.publish(t, mem)
	}
	window := strconv.Itoa(7*weeks + 7)
	for _, by := range []stats.Grouping{stats.GroupDay, stats.GroupWeek} {
		t.Run("detail by "+string(by), func(t *testing.T) {
			t.Parallel()
			page := mustRunStats(t, env, 100, "--by", string(by), "--view", "detail", "--days", window)
			hint := hintCommand(t, page)
			if want := "(all in --json --by " + string(by) + ")"; !strings.Contains(page, want) {
				t.Errorf("the detail screen lacks %q:\n%s", want, page)
			}
			doc := runHint(t, env, hint, "--days", window)
			if doc.Groups == nil || doc.Groups.By != by || len(doc.Groups.Rows) != weeks {
				t.Fatalf("%v does not list all %d rows: %+v", hint, weeks, doc.Groups)
			}
			// The screen keeps the newest rows and says how many earlier ones
			// it left out, and those are in the JSON.
			if want := "5 earlier rows not shown"; !strings.Contains(page, want) {
				t.Errorf("the detail screen lacks %q:\n%s", want, page)
			}
			if oldest := doc.Groups.Rows[0].Key; strings.Contains(page, oldest) {
				t.Errorf("the oldest row, %s, is on the screen that says it left out 5", oldest)
			}
			if newest := doc.Groups.Rows[weeks-1].Key; !strings.Contains(page, newest) {
				t.Errorf("the newest row, %s, is not on the screen", newest)
			}
		})
	}
}

// The hint lines never run past the terminal, at any width the screens adapt
// to, with or without color and in ASCII, and they wrap rather than lose the
// command they name.
func TestStatsAllInHintsFitTheTerminal(t *testing.T) {
	t.Parallel()
	s := realisticStats()
	s.Projects = nil
	s.Models = nil
	for i := range statsMaxListRows + 12345 {
		name := fmt.Sprintf("p%05d", i)
		s.Projects = append(s.Projects, stats.Project{Name: name, Sessions: 1, Tokens: i64(1000), Cost: usd(float64(1000 - i%900))})
		s.Models = append(s.Models, stats.ModelRow{Label: "m" + name, Sessions: 1, Tokens: 1000, Priced: true, Cost: usd(1)})
	}
	s.TotalProjects = len(s.Projects)
	rows := make([]stats.Group, 0, statsMaxGroupRows+100000)
	for i := range statsMaxGroupRows + 100000 {
		rows = append(rows, stats.Group{Key: fmt.Sprintf("g%d", i), Sessions: 1})
	}
	type mode struct {
		color bool
		ascii bool
	}
	modes := []mode{{false, false}, {true, false}, {false, true}}
	// Every width up to where the layouts change, then a spread to 250.
	var widths []int
	for w := statsMinWidth; w <= 100; w++ {
		widths = append(widths, w)
	}
	widths = append(widths, 111, 120, 137, 160, 200, 250)
	for _, tc := range []struct {
		name string
		page statsPage
		by   stats.Grouping
		want string
	}{
		{"projects", pageProjects, stats.GroupNone, "--json --by project"},
		{"models", pageModels, stats.GroupNone, "--json"},
		{"detail by project", pageDetail, stats.GroupProject, "--json --by project"},
		{"detail by day", pageDetail, stats.GroupDay, "--json --by day"},
		{"detail by week", pageDetail, stats.GroupWeek, "--json --by week"},
		{"detail by month", pageDetail, stats.GroupMonth, "--json --by month"},
	} {
		s := s
		if tc.by != stats.GroupNone {
			s.Groups = &stats.Groups{By: tc.by, Rows: rows}
		}
		for _, width := range widths {
			for _, mode := range modes {
				out := stripANSI(strings.Join(pageLines(tc.page, s, width, mode.color, mode.ascii), "\n"))
				checkTerminalSafe(t, fmt.Sprintf("%s %d", tc.name, width), out, width)
				// Wrapping may break the command over lines, never lose it.
				if flat := strings.Join(strings.Fields(out), " "); !strings.Contains(flat, "(all in "+tc.want+")") {
					t.Fatalf("%s at %d columns lost the command %q:\n%s", tc.name, width, tc.want, out)
				}
			}
		}
	}
}
