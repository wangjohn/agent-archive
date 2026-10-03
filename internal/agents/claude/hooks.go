package claude

import "github.com/wangjohn/agent-archive/internal/agents/hookconfig"

// Hooks implements pure native hook planning and inspection.
func Hooks() hookconfig.Configurator {
	return hookconfig.Configurator{Spec: hookconfig.Spec{Name: "claude", RootEnvironment: "CLAUDE_CONFIG_DIR", DefaultDirectory: ".claude", Filename: "settings.json", Flat: false, Version: false, Owner: "agent-archive lifecycle capture", PrototypeOwner: "Recording private skill-run evidence", Events: []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "SubagentStop"}}}
}
