package cursor

import "github.com/wangjohn/agent-archive/internal/agents/skillconfig"

// Skills declares native managed and evidence locations without probing the host.
func Skills() skillconfig.Provider {
	return skillconfig.Provider{ManagedSuffix: ".agents/skills", Frontmatter: false, Roots: []skillconfig.Root{{Suffix: ".cursor/skills", Scope: "user_cursor", Project: false}, {Suffix: ".cursor/skills", Scope: "project_cursor", Project: true}}}
}
