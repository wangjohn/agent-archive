package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"strings"
)

// RuntimeDetector observes native session evidence through an injected getter.
type RuntimeDetector struct{}

// Detect returns exact identity or native project-presence fallback.
func (RuntimeDetector) Detect(env agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	value, set := env.LookupEnv("CODEX_THREAD_ID")
	value = strings.TrimSpace(value)
	if !set || value == "" {
		return agentapi.RuntimeObservation{}
	}
	return agentapi.RuntimeObservation{NativeID: value, PresenceKey: "CODEX_THREAD_ID"}
}

// SessionEnvironmentKeys returns native session variables to strip on launch.
// Configuration variables sharing these prefixes are deliberately preserved.
func (RuntimeDetector) SessionEnvironmentKeys() []string {
	return []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_CI", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_PERMISSION_PROFILE", "CODEX_VERSION"}
}
