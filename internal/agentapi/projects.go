package agentapi

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/platform"
	"io/fs"
)

// NativePathEnvironment carries observed locations and operating-system facts.
// Getenv is read-only; providers must never probe host paths while declaring them.
type NativePathEnvironment struct {
	Locations       NativeLocations
	OperatingSystem platform.OS
	Getenv          func(string) string
}

// NativeProjectPaths declares native storage and project conventions. Empty paths
// mean unavailable; callers must not turn them into relative paths by joining them.
type NativeProjectPaths struct {
	Database          string
	WorkspaceStorage  string
	DesktopWorkspaces []string
	Worktrees         []string
}

// NativePathsProvider owns native path inventories without performing host I/O.
type NativePathsProvider interface {
	ProjectPaths(NativePathEnvironment) NativeProjectPaths
}

// NativePathsLookup provides path declarations for real source and project consumers.
type NativePathsLookup interface {
	LookupNativePaths(string) (NativePathsProvider, bool)
	NativePathAgents() []string
}

// MissingWorktreeResolver interprets a native path convention; shared Git and
// configured-project policy decide whether the resulting repository is admissible.
type MissingWorktreeResolver interface{ MissingWorktreeRepository(string) (string, bool) }

// WorktreeLookup projects native conventions separately from filesystem Git policy.
type WorktreeLookup interface {
	WorktreeResolvers() []MissingWorktreeResolver
}

// WorkspaceFiles restricts native workspace interpretation to read-only operations.
type WorkspaceFiles interface {
	ReadDir(string) ([]fs.DirEntry, error)
	ReadFile(string) ([]byte, error)
}

// WorkspaceRequest supplies native evidence and already resolved candidate roots.
type WorkspaceRequest struct {
	Candidates  []string
	Environment NativePathEnvironment
	Files       WorkspaceFiles
	ResolvePath func(string) string
}

// WorkspaceEvidencePurpose distinguishes project files from message metadata.
type WorkspaceEvidencePurpose uint8

// WorkspaceProjectFile and WorkspaceMessageRecord select the native metadata layout being interpreted.
const (
	WorkspaceProjectFile WorkspaceEvidencePurpose = iota + 1
	WorkspaceMessageRecord
)

// WorkspaceEvidence is local raw metadata. It must not be persisted in diagnostics.
type WorkspaceEvidence struct {
	Purpose WorkspaceEvidencePurpose
	Bytes   []byte
}

// WorkspaceResolver owns native workspace naming and metadata interpretation.
type WorkspaceResolver interface {
	OpenWorkspace(context.Context, WorkspaceRequest) (WorkspacePass, error)
	WorkspaceFolders(WorkspaceEvidence) []string
}

// WorkspaceLookup supplies native workspace resolution only where implemented.
type WorkspaceLookup interface {
	LookupWorkspace(string) (WorkspaceResolver, bool)
}

// WorkspacePass retains one compact observed workspace inventory and match cache
// for a planning invocation; it holds no open files or raw message content.
type WorkspacePass interface {
	MatchWorkspace(context.Context, string) ([]string, error)
}
