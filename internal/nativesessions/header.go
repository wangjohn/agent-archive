// Package nativesessions coordinates bounded read-only native discovery.
package nativesessions

import (
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Header holds inspected identity and cwd facts, without import policy.
type Header = agentapi.NativeHeader

// Inspect resolves purpose-specific compatibility inspection through an injected port.
func Inspect(ports agentapi.NativeHeadersLookup, harness, path string, scan func(func([]byte) bool) error) (Header, error) {
	if ports == nil {
		return Header{}, fmt.Errorf("native header lookup required")
	}
	inspector, ok := ports.LookupNativeHeaders(harness)
	if !ok {
		return Header{}, fmt.Errorf("native header inspection unavailable")
	}
	return inspector.InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryImport, Path: path, Scan: scan})
}
