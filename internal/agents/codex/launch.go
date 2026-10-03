// Package codex implements Codex native integration contracts.
package codex

import (
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Launcher implements native launch arguments without probing the host.
type Launcher struct{}

// Executables returns the native CLI candidates in preference order.
func (Launcher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"codex"}, Install: "Codex"}
}

// Args constructs the native argv, rejecting a conflicting prompt separator.
func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	if slices.Contains(r.ExtraArgs, "--") {
		return nil, fmt.Errorf("arguments for codex cannot include `--`: the handoff prompt goes after it")
	}
	args := slices.Concat([]string{"--cd", r.ProjectDir}, r.ExtraArgs)
	args = append(args, "--")
	return append(args, r.Prompt), nil
}
