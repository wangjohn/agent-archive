package claude

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/platform"
	"path/filepath"
	"strings"
)

// ProjectEvidence declares native desktop and worktree conventions.
type ProjectEvidence struct{}

// ProjectPaths declares Claude desktop workspace locations for the observed platform.
func (ProjectEvidence) ProjectPaths(e agentapi.NativePathEnvironment) agentapi.NativeProjectPaths {
	var desktop []string
	if e.OperatingSystem == platform.Darwin && e.Locations.UserHome != "" {
		desktop = []string{filepath.Join(e.Locations.UserHome, "Library", "Application Support", "Claude", "scratch-workspaces")}
	}
	return agentapi.NativeProjectPaths{DesktopWorkspaces: desktop}
}

// MissingWorktreeRepository recovers the repository prefix of a Claude worktree path.
func (ProjectEvidence) MissingWorktreeRepository(dir string) (string, bool) {
	marker := string(filepath.Separator) + filepath.Join(".claude", "worktrees") + string(filepath.Separator)
	i := strings.Index(dir, marker)
	if i <= 0 || len(dir) == i+len(marker) {
		return "", false
	}
	return dir[:i], true
}
