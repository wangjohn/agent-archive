package cursor

import "github.com/wangjohn/agent-archive/internal/agents/hookconfig"

// Hooks implements pure native hook planning and inspection.
func Hooks() hookconfig.Configurator {
	return hookconfig.Configurator{Spec: hookconfig.Spec{Name: "cursor", RootEnvironment: "", DefaultDirectory: ".cursor", Filename: "hooks.json", Flat: true, Version: true, Owner: "agent-archive lifecycle capture", PrototypeOwner: "Recording private skill-run evidence", Events: []string{"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd", "subagentStop"}}}
}
