package stats

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// rowsArchive has eight projects, seven skills (two of them one skill under
// two names), six MCP servers and four model families.
func rowsArchive() []archive.Metadata {
	models := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-4-5", "gpt-6-luna"}
	var sessions []archive.Metadata
	for i := range 8 {
		built := []option{
			project(fmt.Sprintf("proj%d", i)),
			modelTokens(models[i%len(models)], 1000*(i+1), 10, 0, 0),
			mcp(fmt.Sprintf("srv%d", i%6), i+1),
			skill(fmt.Sprintf("skill%d", i%5)),
		}
		if i == 0 {
			built = append(built, skill("plugin:skill0", "acme:extra"))
		}
		sessions = append(sessions, meta(fmt.Sprintf("s%d", i), "claude", day(time.September, 10+i, 9), built...))
	}
	return sessions
}

func TestTopListsKeepTheTopFewByDefaultAndCountTheRest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		topN         int
		wantProjects int
		wantSkills   int
		wantServers  int
	}{
		{"the default is DefaultTopN", 0, DefaultTopN, DefaultTopN, DefaultTopN},
		{"a negative TopN is the default too", -3, DefaultTopN, DefaultTopN, DefaultTopN},
		{"a smaller TopN", 2, 2, 2, 2},
		{"a larger TopN than there are rows", 100, 8, 7, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := opts()
			o.TopN = tc.topN
			got := Compute(rowsArchive(), o)
			if len(got.Projects) != tc.wantProjects || len(got.Skills) != tc.wantSkills || len(got.MCP.Servers) != tc.wantServers {
				t.Fatalf("rows = %d projects, %d skills, %d servers; want %d, %d, %d",
					len(got.Projects), len(got.Skills), len(got.MCP.Servers), tc.wantProjects, tc.wantSkills, tc.wantServers)
			}
			if got.TotalProjects != 8 || got.TotalSkills != 7 || got.MCP.TotalServers != 6 {
				t.Fatalf("totals = %d projects, %d skills, %d servers; want 8, 7, 6", got.TotalProjects, got.TotalSkills, got.MCP.TotalServers)
			}
		})
	}
}

// AllRows lists every row whatever TopN says, in the same order the top list
// has them, so a longer list only ever adds rows at the end.
func TestAllRowsListsEveryRow(t *testing.T) {
	t.Parallel()
	for _, topN := range []int{0, 1, 3, 100} {
		short := opts()
		short.TopN = topN
		all := short
		all.AllRows = true
		want, got := Compute(rowsArchive(), short), Compute(rowsArchive(), all)
		if len(got.Projects) != 8 || len(got.Skills) != 7 || len(got.DisplaySkills) != 6 || len(got.MCP.Servers) != 6 || len(got.Models) != 4 {
			t.Fatalf("TopN %d: rows = %d projects, %d skills, %d display skills, %d servers, %d models; want 8, 7, 6, 6, 4",
				topN, len(got.Projects), len(got.Skills), len(got.DisplaySkills), len(got.MCP.Servers), len(got.Models))
		}
		if got.TotalProjects != 8 || got.TotalSkills != 7 || got.TotalDisplaySkills != 6 || got.MCP.TotalServers != 6 {
			t.Fatalf("TopN %d: totals = %d, %d, %d, %d", topN, got.TotalProjects, got.TotalSkills, got.TotalDisplaySkills, got.MCP.TotalServers)
		}
		if mustJSON(t, got.Projects[:len(want.Projects)]) != mustJSON(t, want.Projects) ||
			!slices.Equal(got.Skills[:len(want.Skills)], want.Skills) ||
			!slices.Equal(got.MCP.Servers[:len(want.MCP.Servers)], want.MCP.Servers) {
			t.Fatalf("TopN %d: the full lists do not start with the top lists", topN)
		}
	}
}

// Everything but the lists and their totals is the same with AllRows: the
// option changes how many rows come back and nothing else.
func TestAllRowsChangesNothingButTheLists(t *testing.T) {
	t.Parallel()
	all := opts()
	all.AllRows = true
	a, b := Compute(rowsArchive(), opts()), Compute(rowsArchive(), all)
	b.Projects, b.Skills, b.DisplaySkills, b.MCP.Servers = a.Projects, a.Skills, a.DisplaySkills, a.MCP.Servers
	if mustJSON(t, a) != mustJSON(t, b) {
		t.Fatalf("AllRows changed more than the lists:\n%s\n%s", mustJSON(t, a), mustJSON(t, b))
	}
}

// Every model family is always listed, whatever TopN says.
func TestModelsAreNeverCut(t *testing.T) {
	t.Parallel()
	o := opts()
	o.TopN = 1
	got := Compute(rowsArchive(), o)
	if len(got.Models) != 4 {
		t.Fatalf("models = %d rows, want every family (4)", len(got.Models))
	}
}

func TestSkillDisplayName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"code-review":                "code-review",
		"anthropic-skills:docs":      "docs",
		"plugin:group:skill":         "group:skill",
		":docs":                      ":docs",
		"docs:":                      "docs:",
		":":                          ":",
		"":                           "",
		"a:b:":                       "b:",
		"café:crème":                 "crème",
		"Anthropic-Skills:Docs":      "Docs",
		"bad\xffbytes:\xfe":          "\xfe",
		"one:two three":              "two three",
		"ends-with-colon-and-more::": ":",
	}
	for in, want := range cases {
		if got := SkillDisplayName(in); got != want {
			t.Errorf("SkillDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Skills that are the same once a plugin prefix is stripped are one display
// row, and a session that used several of them counts once; the recorded
// names stay as they are.
func TestDisplaySkillsMergeDuplicatesAndKeepRecordedNames(t *testing.T) {
	t.Parallel()
	at := day(time.September, 20, 10)
	sessions := []archive.Metadata{
		// Both spellings in one session (a native invocation and a read of the
		// skill's file): one session of "docs".
		meta("a", "claude", at, skill("anthropic-skills:docs", "docs", "review-pr")),
		meta("b", "claude", at, skill("docs")),
		meta("c", "claude", at, skill("anthropic-skills:docs")),
		meta("d", "claude", at, skill("other:docs", "review-pr")),
		meta("e", "claude", at, skill("code-review")),
	}
	got := Compute(sessions, opts())

	wantDisplay := []Skill{{"docs", 4}, {"review-pr", 2}, {"code-review", 1}}
	if !slices.Equal(got.DisplaySkills, wantDisplay) {
		t.Errorf("display skills = %+v, want %+v", got.DisplaySkills, wantDisplay)
	}
	wantRecorded := []Skill{{"anthropic-skills:docs", 2}, {"docs", 2}, {"review-pr", 2}, {"code-review", 1}, {"other:docs", 1}}
	if !slices.Equal(got.Skills, wantRecorded) {
		t.Errorf("skills = %+v, want %+v", got.Skills, wantRecorded)
	}
	if got.TotalSkills != 5 || got.TotalDisplaySkills != 3 {
		t.Errorf("totals = %d recorded, %d display, want 5 and 3", got.TotalSkills, got.TotalDisplaySkills)
	}
	// A merged row is never counted above the sessions there are.
	for _, s := range got.DisplaySkills {
		if s.Sessions > got.Coverage.Sessions {
			t.Errorf("%+v is more sessions than the window has (%d)", s, got.Coverage.Sessions)
		}
	}
}

func TestNoSkillsLeavesBothListsOutOfTheJSON(t *testing.T) {
	t.Parallel()
	got := Compute([]archive.Metadata{meta("a", "claude", day(time.September, 20, 10))}, opts())
	if got.Skills != nil || got.DisplaySkills != nil || got.TotalSkills != 0 || got.TotalDisplaySkills != 0 {
		t.Fatalf("skills = %+v / %+v, totals %d / %d", got.Skills, got.DisplaySkills, got.TotalSkills, got.TotalDisplaySkills)
	}
}
