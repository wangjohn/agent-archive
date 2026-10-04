package builtin

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/skillownership"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	"testing"
)

func TestSkillsConformance(t *testing.T) {
	t.Parallel()
	r := NewBuiltins()
	for _, name := range r.SkillAgents() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, ok := r.LookupSkills(name)
			if !ok {
				t.Fatal("missing provider")
			}
			agenttest.Skills(t, p, []byte(skillownership.Marker+"\narchive command"))
		})
	}
}

func TestSkillsPurposeLocationsPreserveCurrentCoverage(t *testing.T) {
	t.Parallel()
	r := NewBuiltins()
	cursor, _ := r.LookupSkills("cursor")
	codex, _ := r.LookupSkills("codex")
	l := agentapi.SkillLocations{UserHome: "/synthetic/home", ProjectRoot: "/synthetic/project"}
	if cursor.Destination(l).Directory != codex.Destination(l).Directory {
		t.Fatal("shared managed path diverged")
	}
	roots := cursor.EvidenceRoots(l)
	if len(roots) != 2 || roots[0].Path != "/synthetic/home/.cursor/skills" {
		t.Fatalf("Cursor evidence expanded: %+v", roots)
	}
	for _, d := range r.Catalog().All() {
		found := false
		for _, op := range d.Operations {
			if op == agentmeta.Skills {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing projection %s", d.ID)
		}
	}
}
