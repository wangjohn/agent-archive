package cursor_test

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/platform"
	"path/filepath"
	"testing"
)

type OS = platform.OS

type LocationDeps = platform.LocationDeps

const (
	Darwin  = platform.Darwin
	Linux   = platform.Linux
	Unknown = platform.Unknown
)

var otherSystems = []string{"freebsd", "windows", ""}

func envOf(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

type nativeLocationObservation struct {
	OS                     OS
	CursorAppDir           string
	CursorStateDB          string
	CursorWorkspaceStorage string
	ClaudeDesktopScratch   string
	CodexDocuments         string
}

func nativeLocations(system OS, home string, getenv func(string) string, _ LocationDeps) nativeLocationObservation {
	e := agentapi.NativePathEnvironment{OperatingSystem: system, Locations: agentapi.NativeLocations{UserHome: home}, Getenv: getenv}
	c := (cursor.ProjectEvidence{}).ProjectPaths(e)
	var app, claudeDesktop, codexDesktop string
	if c.Database != "" {
		app = filepath.Dir(filepath.Dir(filepath.Dir(c.Database)))
	}
	if d := (claude.ProjectEvidence{}).ProjectPaths(e).DesktopWorkspaces; len(d) > 0 {
		claudeDesktop = d[0]
	}
	if d := (codex.ProjectEvidence{}).ProjectPaths(e).DesktopWorkspaces; len(d) > 0 {
		codexDesktop = d[0]
	}
	return nativeLocationObservation{OS: system, CursorAppDir: app, CursorStateDB: c.Database, CursorWorkspaceStorage: c.WorkspaceStorage, ClaudeDesktopScratch: claudeDesktop, CodexDocuments: codexDesktop}
}

func TestCursorLocations(t *testing.T) {
	t.Parallel()
	const home = "/home/me"
	for _, tc := range []struct {
		name   string
		os     OS
		home   string
		env    map[string]string
		getenv func(string) string // used instead of env when set
		want   string              // CursorAppDir
	}{
		{"macOS", Darwin, home, nil, nil, "/home/me/Library/Application Support/Cursor"},
		{"macOS ignores XDG_CONFIG_HOME", Darwin, home, map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, "/home/me/Library/Application Support/Cursor"},
		{"Linux default", Linux, home, nil, nil, "/home/me/.config/Cursor"},
		{"Linux XDG_CONFIG_HOME", Linux, home, map[string]string{"XDG_CONFIG_HOME": "/xdg/config"}, nil, "/xdg/config/Cursor"},
		{"Linux XDG_CONFIG_HOME with trailing slash", Linux, home, map[string]string{"XDG_CONFIG_HOME": "/xdg/config/"}, nil, "/xdg/config/Cursor"},
		{"Linux empty XDG_CONFIG_HOME", Linux, home, map[string]string{"XDG_CONFIG_HOME": ""}, nil, "/home/me/.config/Cursor"},
		{"Linux relative XDG_CONFIG_HOME is ignored", Linux, home, map[string]string{"XDG_CONFIG_HOME": "cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux dot-relative XDG_CONFIG_HOME is ignored", Linux, home, map[string]string{"XDG_CONFIG_HOME": "./cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux tilde XDG_CONFIG_HOME is ignored", Linux, home, map[string]string{"XDG_CONFIG_HOME": "~/cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux without an environment", Linux, home, nil, func(string) string { return "" }, "/home/me/.config/Cursor"},
		{"Linux with a nil getenv", Linux, home, nil, nil, "/home/me/.config/Cursor"},
		// An unknown system has no Cursor location: it is not quietly given
		// the Linux (VS Code) layout, XDG_CONFIG_HOME or not.
		{"unknown system", Unknown, home, nil, nil, ""},
		{"unknown system ignores XDG_CONFIG_HOME", Unknown, home, map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, ""},
		// Without a home there is no folder under it; never a relative path.
		{"macOS without a home", Darwin, "", nil, nil, ""},
		{"Linux without a home", Linux, "", nil, nil, ""},
		{"Linux without a home still honors an absolute XDG_CONFIG_HOME", Linux, "", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, "/xdg/Cursor"},
	} {
		getenv := tc.getenv
		if getenv == nil && tc.env != nil {
			getenv = envOf(tc.env)
		}
		loc := nativeLocations(tc.os, tc.home, getenv, LocationDeps{})
		if loc.CursorAppDir != tc.want {
			t.Errorf("%s: CursorAppDir = %q, want %q", tc.name, loc.CursorAppDir, tc.want)
		}
		wantDB, wantStorage := "", ""
		if tc.want != "" {
			wantDB = filepath.Join(tc.want, "User", "globalStorage", "state.vscdb")
			wantStorage = filepath.Join(tc.want, "User", "workspaceStorage")
		}
		if loc.CursorStateDB != wantDB {
			t.Errorf("%s: CursorStateDB = %q, want %q", tc.name, loc.CursorStateDB, wantDB)
		}
		if loc.CursorWorkspaceStorage != wantStorage {
			t.Errorf("%s: CursorWorkspaceStorage = %q, want %q", tc.name, loc.CursorWorkspaceStorage, wantStorage)
		}
		if loc.OS != tc.os {
			t.Errorf("%s: OS = %q, want %q", tc.name, loc.OS, tc.os)
		}
	}
}

// Any other name is as unknown as Unknown itself: no Cursor location, not
// the Linux one.

func TestOtherSystemsHaveNoCursorLocation(t *testing.T) {
	t.Parallel()
	for _, name := range otherSystems {
		loc := nativeLocations(OS(name), "/home/me", envOf(map[string]string{"XDG_CONFIG_HOME": "/xdg"}), LocationDeps{})
		if loc.CursorAppDir != "" || loc.CursorStateDB != "" || loc.CursorWorkspaceStorage != "" {
			t.Errorf("%q: Cursor locations %q %q %q, want none", name, loc.CursorAppDir, loc.CursorStateDB, loc.CursorWorkspaceStorage)
		}
	}
}

// The systemd user unit directory is a Linux location under the home, and
// does not follow XDG_CONFIG_HOME (the user manager reads that only from its
// own environment).

func TestDesktopAppFoldersAreMacOnly(t *testing.T) {
	t.Parallel()
	mac := nativeLocations(Darwin, "/Users/me", nil, LocationDeps{})
	if want := "/Users/me/Library/Application Support/Claude/scratch-workspaces"; mac.ClaudeDesktopScratch != want {
		t.Errorf("ClaudeDesktopScratch = %q, want %q", mac.ClaudeDesktopScratch, want)
	}
	if want := "/Users/me/Documents/Codex"; mac.CodexDocuments != want {
		t.Errorf("CodexDocuments = %q, want %q", mac.CodexDocuments, want)
	}
	for _, name := range append([]string{"linux", "unknown"}, otherSystems...) {
		system := OS(name)
		loc := nativeLocations(system, "/home/me", nil, LocationDeps{})
		if loc.ClaudeDesktopScratch != "" || loc.CodexDocuments != "" {
			t.Errorf("%q: desktop app folders %q and %q, want none", system, loc.ClaudeDesktopScratch, loc.CodexDocuments)
		}
	}
	if loc := nativeLocations(Darwin, "", nil, LocationDeps{}); loc.ClaudeDesktopScratch != "" || loc.CodexDocuments != "" {
		t.Errorf("no home: desktop app folders %q and %q, want none", loc.ClaudeDesktopScratch, loc.CodexDocuments)
	}
}

// The temporary directories backfill skips, per system. An unknown system gets
// every one of them: it errs toward treating a folder as temporary.
