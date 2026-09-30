package cursorstore

import (
	"path/filepath"
	"runtime"
	"testing"
)

func envOf(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

// TestAppSupportDir pins Cursor's data folder on both layouts. goos and
// getenv are injected, so every row runs on any OS.
func TestAppSupportDir(t *testing.T) {
	const home = "/home/me"
	for _, tc := range []struct {
		name   string
		goos   string
		env    map[string]string
		getenv func(string) string // used instead of env when set
		want   string
	}{
		{"macOS", "darwin", nil, nil, "/home/me/Library/Application Support/Cursor"},
		{"macOS ignores XDG_CONFIG_HOME", "darwin", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, "/home/me/Library/Application Support/Cursor"},
		{"Linux default", "linux", nil, nil, "/home/me/.config/Cursor"},
		{"Linux XDG_CONFIG_HOME", "linux", map[string]string{"XDG_CONFIG_HOME": "/xdg/config"}, nil, "/xdg/config/Cursor"},
		{"Linux XDG_CONFIG_HOME with trailing slash", "linux", map[string]string{"XDG_CONFIG_HOME": "/xdg/config/"}, nil, "/xdg/config/Cursor"},
		{"Linux empty XDG_CONFIG_HOME", "linux", map[string]string{"XDG_CONFIG_HOME": ""}, nil, "/home/me/.config/Cursor"},
		{"Linux relative XDG_CONFIG_HOME is ignored", "linux", map[string]string{"XDG_CONFIG_HOME": "cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux dot-relative XDG_CONFIG_HOME is ignored", "linux", map[string]string{"XDG_CONFIG_HOME": "./cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux tilde XDG_CONFIG_HOME is ignored", "linux", map[string]string{"XDG_CONFIG_HOME": "~/cfg"}, nil, "/home/me/.config/Cursor"},
		{"Linux without an environment", "linux", nil, func(string) string { return "" }, "/home/me/.config/Cursor"},
		{"other systems use the VS Code layout", "freebsd", nil, nil, "/home/me/.config/Cursor"},
		{"other systems honor XDG_CONFIG_HOME", "freebsd", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, nil, "/xdg/Cursor"},
		{"empty goos uses the VS Code layout", "", nil, nil, "/home/me/.config/Cursor"},
	} {
		getenv := tc.getenv
		if getenv == nil {
			getenv = envOf(tc.env)
		}
		if got := AppSupportDir(home, getenv, tc.goos); got != tc.want {
			t.Errorf("%s: AppSupportDir = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A nil getenv is the same as an empty environment.
	if got := AppSupportDir(home, nil, "linux"); got != "/home/me/.config/Cursor" {
		t.Errorf("nil getenv: %q", got)
	}
}

// TestStateDatabaseFor: the database is under User/globalStorage of the
// data folder, on each layout.
func TestStateDatabaseFor(t *testing.T) {
	for _, tc := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"darwin", nil, "/h/Library/Application Support/Cursor/User/globalStorage/state.vscdb"},
		{"linux", nil, "/h/.config/Cursor/User/globalStorage/state.vscdb"},
		{"linux", map[string]string{"XDG_CONFIG_HOME": "/x"}, "/x/Cursor/User/globalStorage/state.vscdb"},
	} {
		if got := StateDatabaseFor("/h", envOf(tc.env), tc.goos); got != tc.want {
			t.Errorf("%s %v: %q, want %q", tc.goos, tc.env, got, tc.want)
		}
	}
}

// TestStateDatabaseUsesTheRealSystem: StateDatabase is StateDatabaseFor with
// this process's OS and environment. On macOS the result is what it always
// was, byte for byte.
func TestStateDatabaseUsesTheRealSystem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	want := filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	}
	if got := StateDatabase(home); got != want {
		t.Fatalf("StateDatabase = %q, want %q", got, want)
	}
	if runtime.GOOS != "darwin" {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if got, want := StateDatabase(home), filepath.Join(xdg, "Cursor", "User", "globalStorage", "state.vscdb"); got != want {
			t.Fatalf("with XDG_CONFIG_HOME: %q, want %q", got, want)
		}
	}
}
