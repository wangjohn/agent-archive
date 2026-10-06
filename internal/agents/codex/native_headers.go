package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
	"path/filepath"
	"strings"
	"time"
)

// NativeHeaders supplies purpose-specific Codex identity inspection.
type NativeHeaders struct{}

// InspectHeader preserves the native 16-record metadata compatibility scan.
func (NativeHeaders) InspectHeader(r agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	if r.Scan == nil || r.Purpose != agentapi.DiscoveryImport && r.Purpose != agentapi.DiscoveryHandoff && r.Purpose != agentapi.DiscoveryProjects && r.Purpose != agentapi.DiscoveryBoundedProjects {
		return agentapi.NativeHeader{}, fmt.Errorf("invalid native header request")
	}
	if r.Purpose == agentapi.DiscoveryBoundedProjects {
		var h agentapi.NativeHeader
		var decodeErr error
		scanErr := r.Scan(func(line []byte) bool {
			var v struct {
				Type    string `json:"type"`
				Payload struct {
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			decodeErr = json.Unmarshal(line, &v)
			if v.Type == "session_meta" {
				h.Directory = v.Payload.Cwd
			}
			return false
		})
		return h, errors.Join(decodeErr, scanErr)
	}
	var h agentapi.NativeHeader
	seen := 0
	found := false
	err := r.Scan(func(line []byte) bool {
		seen++
		var v struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(line, &v) != nil || v.Type != "session_meta" {
			return seen < 16
		}
		found = true
		var meta codexmeta.CodexMeta
		if json.Unmarshal(v.Payload, &meta) != nil {
			h.IdentityMismatch = true
			return false
		}
		h.NativeID, h.Directory = meta.ID, meta.Cwd
		facts, outcome := meta.Identity(r.Path)
		h.IdentityMismatch = outcome == codexmeta.InvalidIdentity || outcome == codexmeta.InvalidRelationship || outcome == codexmeta.InvalidMetadata
		// Import has older producer/start compatibility than automatic discovery,
		// but neither may flatten a known child, fork, or physical history link.
		if outcome == "" {
			h.CodexIdentity = &facts
			switch {
			case facts.Child:
				// Import reads self-contained child histories through the native provider.
				// Parent links are resolved separately from the child admission.
				if r.Purpose != agentapi.DiscoveryImport || facts.HistoryBase != nil || !strings.EqualFold(facts.RolloutID, facts.ThreadID) {
					h.CapturePending = "child_history_pending"
				}
			case facts.ForkID != "":
				if r.Purpose != agentapi.DiscoveryImport || facts.HistoryBase != nil || !strings.EqualFold(facts.RolloutID, facts.ThreadID) {
					h.CapturePending = "fork_history_pending"
				}
			case facts.HistoryBase != nil || !strings.EqualFold(facts.RolloutID, facts.ThreadID):
				h.CapturePending = "related_history_pending"
			}
		}
		for _, ts := range []string{meta.Timestamp, v.Timestamp} {
			if parsed, e := time.Parse(time.RFC3339Nano, ts); e == nil {
				h.StartedAt = parsed.UTC()
				break
			}
		}
		if r.Purpose == agentapi.DiscoveryHandoff {
			h.SubagentOnly = facts.Child
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
		out = append(out, agentapi.NativeStoreRoot{Harness: "codex", Path: filepath.Join(dir, "archived_sessions"), Historical: true, Depth: 0, Recursive: purpose == agentapi.DiscoveryHandoff || purpose == agentapi.DiscoveryBoundedProjects, Prefix: "rollout-", Suffix: ".jsonl"})
	}
	return out
}

// Discover emits native candidates without archive policy or a second inventory.
func (p NativeHeaders) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (agentapi.DiscoveryReport, error) {
	roots := r.Roots
	if roots == nil {
		roots = p.Roots(r.Locations, r.Purpose)
	}
	return discoveryio.Files(ctx, r, agentmeta.Codex, roots, p, r.Purpose != agentapi.DiscoveryBoundedProjects, emit)
}

// DefaultDirectories declares the native configuration location without host probes.
func (NativeHeaders) DefaultDirectories(home string) []string {
	return []string{filepath.Join(home, ".codex")}
}
