package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
)

// Decoder implements native lifecycle and legacy private intent interpretation.
func Decoder() hookconfig.Decoder {
	return hookconfig.Decoder{Spec: hookconfig.DecoderSpec{Agent: agentmeta.Codex, Events: map[string]agentapi.EventKind{"SessionStart": agentapi.EventStart, "UserPromptSubmit": agentapi.EventTurnStart, "Stop": agentapi.EventStop, "SessionEnd": agentapi.EventStop, "SubagentStop": agentapi.EventSubagent, "Interrupt": agentapi.EventStop}}}
}
