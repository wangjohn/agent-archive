// Package skillconfig implements pure managed skill ownership and plans.
package skillconfig

import (
	"bytes"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"github.com/wangjohn/agent-archive/internal/skillownership"
	"path/filepath"
)

// Root declares native evidence suffix and scope conventions.
type Root struct {
	Suffix, Scope string
	Project       bool
}

// Provider holds native conventions and performs no host operations.
type Provider struct {
	ManagedSuffix string
	Claude        bool
	Roots         []Root
}

// Destination resolves a managed path from caller-supplied locations.
func (p Provider) Destination(l agentapi.SkillLocations) agentapi.SkillDestination {
	root := l.UserHome
	if p.Claude {
		root = l.ClaudeDirectory
		if root == "" {
			root = filepath.Join(l.UserHome, ".claude")
		}
	}
	return agentapi.SkillDestination{Directory: filepath.Join(root, p.ManagedSuffix), Boundary: root, ClaudeFrontmatter: p.Claude}
}

// EvidenceRoots declares current inventory coverage independently of installation.
func (p Provider) EvidenceRoots(l agentapi.SkillLocations) []agentapi.SkillRoot {
	var out []agentapi.SkillRoot
	for _, r := range p.Roots {
		base := l.UserHome
		if r.Project {
			base = l.ProjectRoot
		}
		if base == "" {
			continue
		}
		root := agentapi.SkillRoot{Path: filepath.Join(base, r.Suffix), Scope: r.Scope}
		if r.Project {
			root.ProjectBoundary = base
		} else {
			root.UserBoundary = base
		}
		out = append(out, root)
	}
	return out
}

// Inspect distinguishes health without applying a plan.
func (Provider) Inspect(r agentapi.SkillInspectionRequest) (agentapi.SkillInspection, error) {
	if r.File.ReadError != nil {
		return agentapi.SkillInspection{State: agentapi.HookUnreadable, Reason: "unreadable"}, r.File.ReadError
	}
	if !r.File.Present {
		return agentapi.SkillInspection{State: agentapi.HookAbsent}, nil
	}
	if !r.File.Regular || !skillownership.Owned(r.File.Bytes, r.DataHome) {
		return agentapi.SkillInspection{State: agentapi.HookForeign}, nil
	}
	return agentapi.SkillInspection{State: agentapi.HookOwned, Stale: len(r.Content) > 0 && !bytes.Equal(r.File.Bytes, r.Content)}, nil
}

// Plan returns expected-byte changes; shared transactions apply them.
func (p Provider) Plan(r agentapi.SkillPlanRequest) ([]filechange.Change, error) {
	if r.Action != agentapi.SkillInstall && r.Action != agentapi.SkillRefresh && r.Action != agentapi.SkillRemove {
		return nil, fmt.Errorf("invalid skill action")
	}
	s, err := p.Inspect(agentapi.SkillInspectionRequest{File: r.File, Content: r.Content, DataHome: r.DataHome})
	if err != nil {
		return nil, err
	}
	if s.State == agentapi.HookForeign {
		return nil, nil
	}
	if r.Action == agentapi.SkillRemove && s.State == agentapi.HookAbsent {
		return nil, nil
	}
	if r.Action != agentapi.SkillRemove && s.State == agentapi.HookOwned && !s.Stale {
		return nil, nil
	}
	mode := r.File.Mode
	if !r.File.Present {
		mode = 0600
	}
	return []filechange.Change{{Path: r.File.Path, Before: r.File.Bytes, After: r.Content, Existed: r.File.Present, Mode: mode, Delete: r.Action == agentapi.SkillRemove}}, nil
}
