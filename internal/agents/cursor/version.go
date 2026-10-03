package cursor

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"path/filepath"
)

// VersionInspector owns native installed-version bundle observation.
type VersionInspector struct{}

// ObserveVersion preserves unknown off macOS and reads only declared bundles.
func (VersionInspector) ObserveVersion(e agentapi.VersionEnvironment) agentapi.ApplicationDiscovery {
	if !e.MacOS {
		return agentapi.ApplicationDiscovery{VersionState: "unknown"}
	}
	for _, bundle := range []string{"/Applications/Cursor.app", filepath.Join(e.UserHome, "Applications", "Cursor.app")} {
		present, dir := e.Host.Exists(bundle)
		if !present || !dir {
			continue
		}
		plist := filepath.Join(bundle, "Contents", "Info.plist")
		version, ok := e.Host.ProbeVersion(agentapi.VersionProbe{Kind: agentapi.VersionBundle, Path: plist})
		if ok {
			return agentapi.ApplicationDiscovery{Installed: true, Version: version, VersionSource: plist + ":CFBundleShortVersionString", VersionKind: "app_bundle", VersionState: "observed"}
		}
		return agentapi.ApplicationDiscovery{Installed: true, VersionSource: plist, VersionKind: "app_bundle", VersionState: "unknown"}
	}
	return agentapi.ApplicationDiscovery{VersionState: "absent"}
}
