// Package cursor implements Cursor's CLI native integration contracts.
package cursor

import (
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Launcher implements native launch arguments without probing the host.
type Launcher struct{}

// Executables returns the native CLI candidates in preference order.
func (Launcher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"agent", "cursor-agent"}, Install: "Cursor's CLI"}
}

// Args constructs the native argv, rejecting a conflicting prompt separator.
func (Launcher) Args(r agentapi.LaunchRequest) ([]string, error) {
	if slices.Contains(r.ExtraArgs, "--") {
		return nil, fmt.Errorf("arguments for cursor cannot include `--`: the handoff prompt goes after it")
	}
	args := slices.Concat([]string{"--workspace", r.ProjectDir}, r.ExtraArgs)

	return append(args, r.Prompt), nil
}
