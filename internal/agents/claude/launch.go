// Package claude implements Claude Code native integration contracts.
package claude

import (
	"fmt"
	"path/filepath"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Launcher implements native launch arguments without probing the host.
type Launcher struct{}

// Executables returns the native CLI candidates in preference order.
func (Launcher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"claude"}, Install: "Claude Code"}
}

// Args constructs the native argv, rejecting a conflicting prompt separator.
func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	if slices.Contains(r.ExtraArgs, "--") {
		return nil, fmt.Errorf("arguments for claude cannot include `--`: the handoff prompt goes after it")
	}
	args := slices.Concat([]string{"--add-dir", filepath.Dir(r.HandoffPath)}, r.ExtraArgs)
	args = append(args, "--")
	return append(args, r.Prompt), nil
}
