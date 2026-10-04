package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/versionprobe"
	"path/filepath"
)

// VersionInspector owns native installed-version candidates.
type VersionInspector struct{}

// Candidates lists native executables in their current preference order.
func (VersionInspector) Candidates(e agentapi.VersionEnvironment) []string {
	if !e.MacOS {
		return []string{"codex"}
	}
	return []string{"/Applications/Codex.app/Contents/Resources/codex", filepath.Join(e.UserHome, "Applications", "Codex.app", "Contents", "Resources", "codex"), "codex", "/Applications/ChatGPT.app/Contents/Resources/codex", filepath.Join(e.UserHome, "Applications", "ChatGPT.app", "Contents", "Resources", "codex")}
}

// ObserveVersion performs only injected read/probe operations.
func (p VersionInspector) ObserveVersion(e agentapi.VersionEnvironment) agentapi.ApplicationDiscovery {
	return versionprobe.Commands(e.Host, "codex", p.Candidates(e))
}
