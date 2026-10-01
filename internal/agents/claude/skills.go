package claude

import "github.com/wangjohn/agent-archive/internal/agents/skillconfig"

// Skills declares native managed and evidence locations without probing the host.
func Skills() skillconfig.Provider {
	return skillconfig.Provider{ManagedSuffix: "skills", Claude: true, Roots: []skillconfig.Root{{Suffix: ".claude/skills", Scope: "user_claude", Project: false}, {Suffix: ".claude/skills", Scope: "project_claude", Project: true}}}
}
