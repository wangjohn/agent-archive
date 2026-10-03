package cursor

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"strings"
)

// RuntimeDetector observes native session evidence through an injected getter.
type RuntimeDetector struct{}

// Detect returns exact identity or native project-presence fallback.
func (RuntimeDetector) Detect(env agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	value, set := env.LookupEnv("CURSOR_AGENT")
	value = strings.TrimSpace(value)
	if !set || value == "" {
		return agentapi.RuntimeObservation{}
	}
	return agentapi.RuntimeObservation{PresenceKey: "CURSOR_AGENT", ProjectLatest: true}
}

// SessionEnvironmentKeys returns native session variables to strip on launch.
// Configuration variables sharing these prefixes are deliberately preserved.
func (RuntimeDetector) SessionEnvironmentKeys() []string {
	return []string{"CURSOR_AGENT"}
}
