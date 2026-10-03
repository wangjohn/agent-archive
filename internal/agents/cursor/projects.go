package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/platform"
	"net/url"
	"path/filepath"
	"strings"
)

// ProjectEvidence owns Cursor storage, workspace slug and metadata conventions.
type ProjectEvidence struct{}

// ProjectPaths declares Cursor database, workspace storage and worktree locations.
func (ProjectEvidence) ProjectPaths(e agentapi.NativePathEnvironment) agentapi.NativeProjectPaths {
	var app string
	home := e.Locations.UserHome
	switch e.OperatingSystem {
	case platform.Darwin:
		if home != "" {
			app = filepath.Join(home, "Library", "Application Support", "Cursor")
		}
	case platform.Unknown:
	// No native application data root is declared on an unknown operating system.
	case platform.Linux:
		var xdg string
		if e.Getenv != nil {
			xdg = e.Getenv("XDG_CONFIG_HOME")
		}
		if filepath.IsAbs(xdg) {
			app = filepath.Join(xdg, "Cursor")
		} else if home != "" {
			app = filepath.Join(home, ".config", "Cursor")
		}
	}
	worktrees := []string{filepath.Join(home, ".cursor", "worktrees")}
	if app == "" {
		return agentapi.NativeProjectPaths{Worktrees: worktrees}
	}
	return agentapi.NativeProjectPaths{Worktrees: worktrees, Database: filepath.Join(app, "User", "globalStorage", "state.vscdb"), WorkspaceStorage: filepath.Join(app, "User", "workspaceStorage")}

}

// OpenWorkspace prepares one compact native workspace inventory for this planning pass.
func (ProjectEvidence) OpenWorkspace(ctx context.Context, r agentapi.WorkspaceRequest) (agentapi.WorkspacePass, error) {
	if r.Files == nil || r.ResolvePath == nil {
		return nil, fmt.Errorf("workspace dependencies required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m := newCursorMatcher(ctx, r, r.Candidates)
	return m, ctx.Err()
}

func (m *cursorMatcher) MatchWorkspace(ctx context.Context, key string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.context = ctx
	m.match(key)
	return m.cache[key], ctx.Err()
}

// WorkspaceFolders extracts checkout directories from the requested native metadata layout.
func (ProjectEvidence) WorkspaceFolders(e agentapi.WorkspaceEvidence) []string {
	switch e.Purpose {
	case agentapi.WorkspaceProjectFile:
		if folder := workspaceJSONFolder(e.Bytes); folder != "" {
			return []string{folder}
		}
	case agentapi.WorkspaceMessageRecord:
		var m struct {
			WorkspaceURIs []json.RawMessage `json:"workspaceUris"`
		}
		if json.Unmarshal(e.Bytes, &m) != nil {
			return nil
		}
		var out []string
		seen := map[string]bool{}
		for _, raw := range m.WorkspaceURIs {
			if folder := composerWorkspaceFolder(raw); folder != "" && !seen[folder] {
				seen[folder] = true
				out = append(out, folder)
			}
		}
		return out
	}
	return nil
}

// cursorSlug is the folder name Cursor gives a workspace under
// ~/.cursor/projects: the absolute path without its leading separator, with
// every character other than an ASCII letter or digit replaced by '-'. It
// cannot be reversed reliably, so candidates are converted and compared.
func cursorSlug(path string) string {
	path = strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	return slugName(path)
}

func slugName(name string) string {
	b := []byte(name)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// cursorMatcher resolves Cursor project slugs to folders.
type cursorMatcher struct {
	context context.Context
	request agentapi.WorkspaceRequest
	// candidates are folders a slug may name: configured roots, roots and
	// working directories from Claude Code and Codex sessions, and Cursor's
	// own workspaceStorage folders.
	candidates []string
	cache      map[string][]string
}

// cursorWalkBudget bounds how many directories one slug's file system walk
// lists.
const cursorWalkBudget = 4096

func newCursorMatcher(ctx context.Context, request agentapi.WorkspaceRequest, candidates []string) *cursorMatcher {
	m := &cursorMatcher{context: ctx, request: request, cache: map[string][]string{}}
	m.candidates = append(m.candidates, candidates...)
	m.candidates = append(m.candidates, cursorWorkspaceFolders(ctx, request)...)
	return m
}

// cursorWorkspaceStorage is Cursor's folder of per-workspace state:
// User/workspaceStorage in its data folder (platform.Locations), "" where
// the environment's operating system has no known place for it.
func cursorWorkspaceStorage(request agentapi.WorkspaceRequest) string {
	return (ProjectEvidence{}).ProjectPaths(request.Environment).WorkspaceStorage
}

// cursorWorkspaceFolders reads the folder of each
// <Cursor data folder>/User/workspaceStorage/*/workspace.json
// (~/Library/Application Support/Cursor on macOS, ~/.config/Cursor on
// Linux).
func cursorWorkspaceFolders(ctx context.Context, request agentapi.WorkspaceRequest) []string {
	storage := cursorWorkspaceStorage(request)
	if storage == "" {
		return nil
	}
	entries, err := request.Files.ReadDir(storage)
	if err != nil {
		return nil
	}
	var folders []string
	for _, e := range entries {
		if ctx.Err() != nil {
			return nil
		}
		if !e.IsDir() {
			continue
		}
		data, err := request.Files.ReadFile(filepath.Join(storage, e.Name(), "workspace.json"))
		if err != nil {
			continue
		}
		if folder := workspaceJSONFolder(data); folder != "" {
			folders = append(folders, folder)
		}
	}
	return folders
}

// workspaceJSONFolder is the local folder a workspace.json names, "" when it
// names none (a multi-root or remote workspace).
func workspaceJSONFolder(data []byte) string {
	var ws struct {
		Folder string `json:"folder"`
	}
	if json.Unmarshal(data, &ws) != nil || ws.Folder == "" {
		return ""
	}
	u, err := url.Parse(ws.Folder)
	if err != nil || u.Scheme != "file" || !filepath.IsAbs(u.Path) {
		return ""
	}
	return filepath.Clean(u.Path)
}

// match returns the one folder slug names, or false when none or several do.
func (m *cursorMatcher) match(slug string) (string, bool) {
	matches, ok := m.cache[slug]
	if !ok {
		seen := map[string]bool{}
		add := func(p string) {
			key := m.request.ResolvePath(p)
			if !seen[key] {
				seen[key] = true
				matches = append(matches, p)
			}
		}
		for _, c := range m.candidates {
			if cursorSlug(c) == slug {
				add(c)
			}
		}
		budget := cursorWalkBudget
		m.walk(string(filepath.Separator), slug, &budget, add)
		m.cache[slug] = matches
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

// walk finds existing folders under dir whose slug is rest, trying each '-'
// in the slug as a path separator. Each directory level is listed once and
// its entries compared by their own slug, so '.', '_', and '-' in real names
// all match.
func (m *cursorMatcher) walk(dir, rest string, budget *int, found func(string)) {
	if *budget <= 0 || m.context.Err() != nil {
		return
	}
	*budget--
	entries, err := m.request.Files.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := slugName(e.Name())
		switch {
		case name == rest:
			found(filepath.Join(dir, e.Name()))
		case strings.HasPrefix(rest, name+"-"):
			m.walk(filepath.Join(dir, e.Name()), rest[len(name)+1:], budget, found)
		}
	}
}

// RecognizesTranscriptPath recognizes Cursor's native transcript directory component.
func (ProjectEvidence) RecognizesTranscriptPath(_ agentapi.NativePathEnvironment, path string) bool {
	return filepath.IsAbs(path) && strings.Contains(filepath.ToSlash(filepath.Clean(path)), "/agent-transcripts/")
}
