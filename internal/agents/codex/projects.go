package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"path/filepath"
)

// ProjectEvidence declares native desktop and worktree locations.
type ProjectEvidence struct{}

func (ProjectEvidence) ProjectPaths(e agentapi.NativePathEnvironment) agentapi.NativeProjectPaths {
	var out agentapi.NativeProjectPaths
	if e.OperatingSystem == "darwin" && e.Locations.UserHome != "" {
		out.DesktopWorkspaces = []string{filepath.Join(e.Locations.UserHome, "Documents", "Codex")}
	}
	dirs := e.Locations.Directories
	if dirs == nil {
		dirs = (NativeHeaders{}).DefaultDirectories(e.Locations.UserHome)
	}
	for _, dir := range dirs {
		out.Worktrees = append(out.Worktrees, filepath.Join(dir, "worktrees"))
	}
	return out
}
