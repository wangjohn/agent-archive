package claude

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"strings"
)

// RuntimeDetector observes native session evidence through an injected getter.
type RuntimeDetector struct{}

// Detect returns exact identity or native project-presence fallback.
func (RuntimeDetector) Detect(env agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	value, set := env.LookupEnv("CLAUDE_CODE_SESSION_ID")
	value = strings.TrimSpace(value)
	if !set || value == "" {
		return agentapi.RuntimeObservation{}
	}
	return agentapi.RuntimeObservation{NativeID: value, PresenceKey: "CLAUDE_CODE_SESSION_ID"}
}

// SessionEnvironmentKeys returns native session variables to strip on launch.
// Configuration variables sharing these prefixes are deliberately preserved.
func (RuntimeDetector) SessionEnvironmentKeys() []string {
	return []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_PID", "CLAUDE_EFFORT", "AI_AGENT"}
}
