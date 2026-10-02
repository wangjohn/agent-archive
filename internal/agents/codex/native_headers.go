package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// NativeHeaders supplies purpose-specific Codex identity inspection.
type NativeHeaders struct{}

var rolloutUUID = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// InspectHeader preserves the native 16-record metadata compatibility scan.
func (NativeHeaders) InspectHeader(r agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	if r.Scan == nil || r.Purpose != agentapi.DiscoveryImport && r.Purpose != agentapi.DiscoveryHandoff && r.Purpose != agentapi.DiscoveryProjects {
		return agentapi.NativeHeader{}, fmt.Errorf("invalid native header request")
	}
	var h agentapi.NativeHeader
	seen := 0
	found := false
	err := r.Scan(func(line []byte) bool {
		seen++
		var v struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				ID        string          `json:"id"`
				SessionID string          `json:"session_id"`
				Timestamp string          `json:"timestamp"`
				Cwd       string          `json:"cwd"`
				Source    json.RawMessage `json:"source"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &v) != nil || v.Type != "session_meta" {
			return seen < 16
		}
		found = true
		h.NativeID = v.Payload.ID
		h.Directory = v.Payload.Cwd
		m := rolloutUUID.FindStringSubmatch(filepath.Base(r.Path))
		fileID := ""
		if m != nil {
			fileID = m[1]
		}
		if v.Payload.ID == "" || v.Payload.SessionID != "" && v.Payload.SessionID != v.Payload.ID || fileID == "" || !strings.EqualFold(fileID, v.Payload.ID) {
			h.IdentityMismatch = true
		}
		for _, ts := range []string{v.Payload.Timestamp, v.Timestamp} {
			if parsed, e := time.Parse(time.RFC3339Nano, ts); e == nil {
				h.StartedAt = parsed.UTC()
				break
			}
		}
		if r.Purpose == agentapi.DiscoveryHandoff {
			var source struct {
				Subagent json.RawMessage `json:"subagent"`
			}
			if json.Unmarshal(v.Payload.Source, &source) == nil && len(source.Subagent) > 0 {
				h.SubagentOnly = true
			}
		}
		return false
	})
	if !found {
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
		out = append(out, agentapi.NativeStoreRoot{Harness: "codex", Path: filepath.Join(dir, "sessions"), Priority: 1, Depth: -1, Recursive: true, Prefix: "rollout-", Suffix: ".jsonl"})
		out = append(out, agentapi.NativeStoreRoot{Harness: "codex", Path: filepath.Join(dir, "archived_sessions"), Historical: true, Depth: 0, Recursive: purpose == agentapi.DiscoveryHandoff, Prefix: "rollout-", Suffix: ".jsonl"})
	}
	return out
}

// Discover emits native candidates without archive policy or a second inventory.
func (p NativeHeaders) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (agentapi.DiscoveryReport, error) {
	roots := r.Roots
	if roots == nil {
		roots = p.Roots(r.Locations, r.Purpose)
	}
	return discoveryio.Files(ctx, r, agentmeta.Codex, roots, p, true, emit)
}

// DefaultDirectories declares the native configuration location without host probes.
func (NativeHeaders) DefaultDirectories(home string) []string {
	return []string{filepath.Join(home, ".codex")}
}
