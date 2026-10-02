package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
	"path/filepath"
	"strings"
	"time"
)

// NativeHeaders supplies purpose-specific Claude identity inspection.
type NativeHeaders struct{}

// InspectHeader preserves import compatibility and stricter native selection.
func (NativeHeaders) InspectHeader(r agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	if r.Scan == nil || r.Purpose != agentapi.DiscoveryImport && r.Purpose != agentapi.DiscoveryHandoff && r.Purpose != agentapi.DiscoveryProjects {
		return agentapi.NativeHeader{}, fmt.Errorf("invalid native header request")
	}
	stem := strings.TrimSuffix(filepath.Base(r.Path), ".jsonl")
	if r.Purpose != agentapi.DiscoveryHandoff {
		h := agentapi.NativeHeader{NativeID: stem}
		err := r.Scan(func(line []byte) bool {
			var v struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &v) == nil && v.Cwd != "" {
				h.Directory = v.Cwd
				return false
			}
			return true
		})
		return h, err
	}
	var h agentapi.NativeHeader
	sidechain := false
	err := r.Scan(func(line []byte) bool {
		var v struct {
			ID        string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
			Sidechain bool   `json:"isSidechain"`
		}
		if json.Unmarshal(line, &v) != nil {
			return true
		}
		if v.Sidechain {
			sidechain = true
			return true
		}
		if v.ID != "" {
			if h.NativeID != "" && h.NativeID != v.ID {
				h.IdentityMismatch = true
			}
			h.NativeID = v.ID
		}
		if h.Directory == "" {
			h.Directory = v.Cwd
		}
		if h.StartedAt.IsZero() {
			h.StartedAt, _ = time.Parse(time.RFC3339Nano, v.Timestamp)
		}
		return true
	})
	h.SubagentOnly = h.NativeID == "" && sidechain
	if h.NativeID == "" || h.NativeID != stem {
		h.IdentityMismatch = true
	}
	return h, err
}

// Roots declares purpose-specific native store traversal.
func (p NativeHeaders) Roots(l agentapi.NativeLocations, purpose agentapi.DiscoveryPurpose) []agentapi.NativeStoreRoot {
	dirs := l.Directories
	if dirs == nil {
		dirs = p.DefaultDirectories(l.UserHome)
	}
	var out []agentapi.NativeStoreRoot
	for _, dir := range dirs {
		out = append(out, agentapi.NativeStoreRoot{Harness: "claude", Path: filepath.Join(dir, "projects"), Depth: 1, Recursive: false, Prefix: "", Suffix: ".jsonl"})
	}
	return out
}

// Discover emits native candidates without archive policy or a second inventory.
func (p NativeHeaders) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (agentapi.DiscoveryReport, error) {
	roots := r.Roots
	if roots == nil {
		roots = p.Roots(r.Locations, r.Purpose)
	}
	return discoveryio.Files(ctx, r, agentmeta.Claude, roots, p, false, emit)
}

// DefaultDirectories declares the native configuration location without host probes.
func (NativeHeaders) DefaultDirectories(home string) []string {
	return []string{filepath.Join(home, ".claude")}
}
