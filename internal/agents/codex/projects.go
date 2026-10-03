package codex

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/platform"
	"path/filepath"
)

// ProjectEvidence declares native desktop and worktree locations.
type ProjectEvidence struct{}

// ProjectPaths declares Codex worktree locations from the observed user home.
func (ProjectEvidence) ProjectPaths(e agentapi.NativePathEnvironment) agentapi.NativeProjectPaths {
	var desktop []string
	if e.OperatingSystem == platform.Darwin && e.Locations.UserHome != "" {
		desktop = []string{filepath.Join(e.Locations.UserHome, "Documents", "Codex")}
	}
	out := agentapi.NativeProjectPaths{DesktopWorkspaces: desktop}
	dirs := e.Locations.Directories
	if dirs == nil {
		dirs = (NativeHeaders{}).DefaultDirectories(e.Locations.UserHome)
	}
	for _, dir := range dirs {
		out.Worktrees = append(out.Worktrees, filepath.Join(dir, "worktrees"))
	}
	return out
}

// RecognizesTranscriptPath recognizes files under this integration's observed roots.
func (ProjectEvidence) RecognizesTranscriptPath(e agentapi.NativePathEnvironment, path string) bool {
	dirs := e.Locations.Directories
	if dirs == nil {
		dirs = (NativeHeaders{}).DefaultDirectories(e.Locations.UserHome)
	}
	for _, dir := range dirs {
		if filepath.IsAbs(dir) && local.PathWithin(path, dir) {
			return true
		}
	}
	return false
}
