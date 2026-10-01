package cursor

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
)

// Decoder implements native lifecycle and legacy private intent interpretation.
func Decoder() hookconfig.Decoder {
	return hookconfig.Decoder{Spec: hookconfig.DecoderSpec{Agent: agentmeta.ID("cursor"), PromptStarts: true, NullPathFresh: true, OwnedFilename: true, NativeObservations: true, Followups: map[string]bool{"afterAgentResponse": true, "stop": true}, StatusFields: map[string]string{"stop": "status", "sessionEnd": "reason"}, StatusValues: map[string][]string{"stop": {"completed", "aborted", "error"}, "sessionEnd": {"completed", "aborted", "error", "window_close", "user_close"}}, Events: map[string]agentapi.EventKind{"sessionStart": agentapi.EventStart, "beforeSubmitPrompt": agentapi.EventTurnStart, "afterAgentResponse": agentapi.EventResponse, "stop": agentapi.EventStop, "sessionEnd": agentapi.EventStop, "subagentStop": agentapi.EventSubagent}}}
}
