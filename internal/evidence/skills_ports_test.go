package evidence

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func observeBuiltinSkills(o SkillOptions) ([]archive.SupplementalEvidence, error) {
	if p, ok := builtin.NewBuiltins().LookupSkills(o.Harness); ok {
		o.Locations = p.EvidenceRoots(agentapi.SkillLocations{UserHome: o.UserHome, ProjectRoot: o.ProjectRoot})
	}
	return ObserveSkills(o)
}
