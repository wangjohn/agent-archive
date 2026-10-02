package claude

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"path/filepath"
	"strings"
)

// ProjectEvidence declares native desktop and worktree conventions.
type ProjectEvidence struct{}

func (ProjectEvidence) ProjectPaths(e agentapi.NativePathEnvironment) agentapi.NativeProjectPaths {
	var out agentapi.NativeProjectPaths
	if e.OperatingSystem == "darwin" && e.Locations.UserHome != "" {
		out.DesktopWorkspaces = []string{filepath.Join(e.Locations.UserHome, "Library", "Application Support", "Claude", "scratch-workspaces")}
	}
	return out
}
func (ProjectEvidence) MissingWorktreeRepository(dir string) (string, bool) {
	marker := string(filepath.Separator) + filepath.Join(".claude", "worktrees") + string(filepath.Separator)
	i := strings.Index(dir, marker)
	if i <= 0 || len(dir) == i+len(marker) {
		return "", false
	}
	return dir[:i], true
}
