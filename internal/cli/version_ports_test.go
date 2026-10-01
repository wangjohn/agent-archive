package cli

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/versioninfo"
	"github.com/wangjohn/agent-archive/internal/versionprobe"
)

func versionEnvironment(home string, system platform.OS) agentapi.VersionEnvironment {
	return agentapi.VersionEnvironment{UserHome: home, MacOS: system == platform.Darwin, Host: versionHost{}}
}
func argvCandidates(paths []string) [][]string {
	var out [][]string
	for _, path := range paths {
		out = append(out, []string{path, "--version"})
	}
	return out
}
func claudeVersionCandidates(home string, system platform.OS) [][]string {
	return argvCandidates(claude.VersionInspector{}.Candidates(versionEnvironment(home, system)))
}
func codexVersionCandidates(home string, system platform.OS) [][]string {
	return argvCandidates(codex.VersionInspector{}.Candidates(versionEnvironment(home, system)))
}
func claudeDesktopBundledCLIs(home string) []string {
	return claude.VersionInspector{}.BundledCLIs(versionEnvironment(home, platform.Darwin))
}
func discoverCursorVersion(home string, system platform.OS) applicationDiscovery {
	return cursor.VersionInspector{}.ObserveVersion(versionEnvironment(home, system))
}
func discoverCommandVersion(name string, candidates [][]string) applicationDiscovery {
	var paths []string
	for _, c := range candidates {
		paths = append(paths, c[0])
	}
	return versionprobe.Commands(versionHost{}, name, paths)
}
func compareDottedVersions(a, b string) int { return versioninfo.Compare(a, b) }

var versionDirPattern = versioninfo.DirectoryPattern

func discoverApplicationsFor(userHome string, system platform.OS) map[string]applicationDiscovery {
	return discoverApplicationsWith(productionAgents, userHome, system)
}
