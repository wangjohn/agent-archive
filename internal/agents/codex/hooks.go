package codex

import "github.com/wangjohn/agent-archive/internal/agents/hookconfig"

// Hooks implements pure native hook planning and inspection.
func Hooks() hookconfig.Configurator {
	return hookconfig.Configurator{Spec: hookconfig.Spec{Name: "codex", RootEnvironment: "CODEX_HOME", DefaultDirectory: ".codex", Filename: "hooks.json", Flat: false, Version: false, Owner: "agent-archive lifecycle capture", PrototypeOwner: "Recording private skill-run evidence", Events: []string{"SessionStart", "UserPromptSubmit", "Stop", "Interrupt", "SessionEnd", "SubagentStop"}}}
}
