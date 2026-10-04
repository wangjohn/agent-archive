package claude

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/versioninfo"
	"github.com/wangjohn/agent-archive/internal/versionprobe"
	"path/filepath"
	"sort"
)

// VersionInspector owns native installed-version candidates.
type VersionInspector struct{}

// BundledCLIs observes native version directories, newest numeric version first.
func (VersionInspector) BundledCLIs(e agentapi.VersionEnvironment) []string {
	root := filepath.Join(e.UserHome, "Library", "Application Support", "Claude", "claude-code")
	var versions []string
	for _, entry := range e.Host.Directories(root) {
		if entry.Directory && versioninfo.DirectoryPattern.MatchString(entry.Name) {
			versions = append(versions, entry.Name)
		}
	}
	sort.SliceStable(versions, func(i, j int) bool { return versioninfo.Compare(versions[i], versions[j]) > 0 })
	paths := make([]string, len(versions))
	for i, v := range versions {
		paths[i] = filepath.Join(root, v, "claude.app", "Contents", "MacOS", "claude")
	}
	return paths
}

// Candidates lists native executables in their current preference order.
func (p VersionInspector) Candidates(e agentapi.VersionEnvironment) []string {
	paths := []string{"claude", filepath.Join(e.UserHome, ".local", "bin", "claude"), filepath.Join(e.UserHome, ".claude", "local", "claude")}
	if e.MacOS {
		paths = append(paths, p.BundledCLIs(e)...)
	}
	return paths
}

// ObserveVersion performs only injected read/probe operations.
func (p VersionInspector) ObserveVersion(e agentapi.VersionEnvironment) agentapi.ApplicationDiscovery {
	return versionprobe.Commands(e.Host, "claude", p.Candidates(e))
}
