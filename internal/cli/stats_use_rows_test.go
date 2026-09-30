package cli

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// The skills and MCP rows name their unit once, after the last count, so a
// row that lists a single skill used in one session reads "1 session" and a
// single server called once "1 call"; a longer row is plural whatever its
// last count is.
func TestStatsUseRowsSayOneSessionAndOneCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		skills []stats.Skill
		mcp    []stats.MCPServer
		want   []string
		absent []string
	}{
		{
			name: "one of each, used once", skills: []stats.Skill{{Name: "loop", Sessions: 1}}, mcp: []stats.MCPServer{{Name: "github", Calls: 1}},
			want: []string{"Skills  loop 1 session", "MCP     github 1 call"}, absent: []string{"1 sessions", "1 calls"},
		},
		{
			name: "one of each, used often", skills: []stats.Skill{{Name: "loop", Sessions: 4}}, mcp: []stats.MCPServer{{Name: "github", Calls: 41}},
			want: []string{"loop 4 sessions", "github 41 calls"},
		},
		{
			name: "several, the last used once", skills: []stats.Skill{{Name: "docs", Sessions: 3}, {Name: "loop", Sessions: 1}},
			mcp:  []stats.MCPServer{{Name: "github", Calls: 3}, {Name: "linear", Calls: 1}},
			want: []string{"docs 3 · loop 1 sessions", "github 3 · linear 1 calls"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := realisticStats()
			s.DisplaySkills, s.TotalDisplaySkills = tc.skills, len(tc.skills)
			s.MCP = &stats.MCP{Servers: tc.mcp, TotalServers: len(tc.mcp)}
			for _, page := range []statsPage{pageOverview, pageDetail} {
				out := strings.Join(pageLines(page, s, 100, false, false), "\n")
				for _, want := range tc.want {
					if !strings.Contains(out, want) {
						t.Errorf("%s: no %q in\n%s", page, want, out)
					}
				}
				for _, absent := range tc.absent {
					if strings.Contains(out, absent) {
						t.Errorf("%s: %q in\n%s", page, absent, out)
					}
				}
			}
		})
	}
}
