// Package versionprobe provides shared native version candidate observation.
package versionprobe

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"path/filepath"
)

// Commands probes declared version candidates in order, retaining unknown versus absent.
func Commands(host agentapi.VersionHost, name string, paths []string) agentapi.ApplicationDiscovery {
	present := false
	for _, path := range paths {
		resolved := path
		if filepath.IsAbs(path) {
			found, dir := host.Exists(path)
			if !found || dir {
				continue
			}
		} else {
			var ok bool
			resolved, ok = host.ResolveExecutable(path)
			if !ok {
				continue
			}
		}
		present = true
		if version, ok := host.ProbeVersion(agentapi.VersionProbe{Kind: agentapi.VersionCLI, Path: resolved}); ok {
			return agentapi.ApplicationDiscovery{Installed: true, Version: version, VersionSource: resolved + " --version", VersionKind: "cli", VersionState: "observed"}
		}
	}
	if present {
		return agentapi.ApplicationDiscovery{Installed: true, VersionSource: name + " --version", VersionKind: "cli", VersionState: "unknown"}
	}
	return agentapi.ApplicationDiscovery{VersionState: "absent"}
}
