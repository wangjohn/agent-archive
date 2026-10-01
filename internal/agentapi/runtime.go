package agentapi

import "github.com/wangjohn/agent-archive/internal/agentmeta"

// RuntimeEnvironment provides only the caller's injected environment.
type RuntimeEnvironment struct {
	LookupEnv func(string) (string, bool)
}

// RuntimeObservation separates exact identity from project-scoped fallback.
// PresenceKey names the positive variable used for noninteractive diagnostics.
type RuntimeObservation struct {
	NativeID      string
	PresenceKey   string
	ProjectLatest bool
}

// RuntimeDetector interprets native environment evidence without host probes.
// Cleanup keys do not necessarily indicate a running agent.
type RuntimeDetector interface {
	Detect(RuntimeEnvironment) RuntimeObservation
	SessionEnvironmentKeys() []string
}

// AgentRuntime pairs native observation with its canonical composition binding.
type AgentRuntime struct {
	Agent agentmeta.ID
	RuntimeObservation
}

// RuntimeLookup is the narrow runtime projection consumed by shared CLI policy.
type RuntimeLookup interface {
	Observations(RuntimeEnvironment) []AgentRuntime
	SessionEnvironmentKeys() []string
}

// AgentLauncher is an implemented launcher bound to its canonical identity.
type AgentLauncher struct {
	Agent    agentmeta.ID
	Launcher Launcher
}

// LauncherLookup resolves just the launch operation, without a registry dependency.
type LauncherLookup interface {
	Launcher(string) (Launcher, bool)
	Launchers() []AgentLauncher
}
