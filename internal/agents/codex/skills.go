package codex

import "github.com/wangjohn/agent-archive/internal/agents/skillconfig"

// Skills declares native managed and evidence locations without probing the host.
func Skills() skillconfig.Provider {
	return skillconfig.Provider{ManagedSuffix: ".agents/skills", Frontmatter: false, Roots: []skillconfig.Root{{Suffix: ".agents/skills", Scope: "user_agents", Project: false}, {Suffix: ".codex/skills", Scope: "user_codex_legacy", Project: false}, {Suffix: ".agents/skills", Scope: "project_agents", Project: true}}}
}
