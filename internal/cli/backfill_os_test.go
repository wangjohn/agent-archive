package cli

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"

	"github.com/wangjohn/agent-archive/internal/platform"
)

// macCursorDatabase is where a Mac keeps Cursor's database under home,
// whatever system the test runs on.
func macCursorDatabase(home string) string {
	return (Env{OS: platform.Darwin, UserHomeDir: func() (string, error) { return home, nil }}).cursorDatabase()
}

// Env.OS decides where backfill, and the collector, look for Cursor's data,
// and which temporary directories backfill skips. On Linux Cursor's database
// is under the XDG config home, read from the Env's environment
// (LookupEnv), not the test process's. On a system the program does not know
// there is no Cursor database to look for.
func TestBackfillEnvironmentFollowsEnvOS(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	xdg := t.TempDir()
	for _, tc := range []struct {
		name   string
		system platform.OS
		xdg    string
		want   string
		temp   []string
		// tmpdir is $TMPDIR in the Env's environment; a non-blank one is
		// appended (trimmed) to the default temporary directories.
		tmpdir string
		// wantTemp is the complete list of temporary directories.
		wantTemp []string
	}{
		{"darwin", platform.Darwin, xdg, filepath.Join(userHome, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"),
			[]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}, "  /private/tmp/mine \n",
			[]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders", "/private/tmp/mine"}},
		{"linux", platform.Linux, "", filepath.Join(userHome, ".config", "Cursor", "User", "globalStorage", "state.vscdb"),
			[]string{"/tmp", "/var/tmp"}, "   ", []string{"/tmp", "/var/tmp"}},
		{"linux with XDG_CONFIG_HOME", platform.Linux, xdg, filepath.Join(xdg, "Cursor", "User", "globalStorage", "state.vscdb"),
			[]string{"/tmp", "/var/tmp"}, "/home/me/scratch", []string{"/tmp", "/var/tmp", "/home/me/scratch"}},
		// The conservative system: no Cursor database even with
		// XDG_CONFIG_HOME set, and every known system's temporary directories.
		{"unknown", platform.Unknown, xdg, "",
			[]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders", "/var/tmp"}, "",
			[]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders", "/var/tmp"}},
	} {
		env := testEnv(t, t.TempDir(), time.Now())
		env.UserHomeDir = func() (string, error) { return userHome, nil }
		env.OS = tc.system
		env.BackfillTempDirs = nil
		env.LookupEnv = func(key string) (string, bool) {
			switch {
			case key == "XDG_CONFIG_HOME" && tc.xdg != "":
				return tc.xdg, true
			case key == "TMPDIR" && tc.tmpdir != "":
				return tc.tmpdir, true
			}
			return "", false
		}
		if got := env.cursorDatabase(); got != tc.want {
			t.Errorf("%s: collector database %q, want %q", tc.name, got, tc.want)
		}
		bf := env.backfillEnvironment(userHome, config.Config{})
		if bf.OS != tc.system {
			t.Errorf("%s: OS %q", tc.name, bf.OS)
		}
		if bf.Getenv == nil {
			t.Fatalf("%s: the backfill environment has no Getenv", tc.name)
		}
		if got := bf.Getenv("XDG_CONFIG_HOME"); got != tc.xdg {
			t.Errorf("%s: XDG_CONFIG_HOME %q, want %q", tc.name, got, tc.xdg)
		}
		if bf.CursorDatabase == nil {
			t.Errorf("%s: no Cursor database reader", tc.name)
		}
		if !equalStrings(bf.TempDirs, tc.wantTemp) {
			t.Errorf("%s: temporary directories %v, want %v", tc.name, bf.TempDirs, tc.wantTemp)
		}
		if !equalStrings(bf.DefaultTempDirs(), tc.temp) {
			t.Errorf("%s: default temporary directories %v, want %v", tc.name, bf.DefaultTempDirs(), tc.temp)
		}
	}
}

// With Env.OS empty, which is production, everything answers for the real
// system: a Mac gets the Library layout and the macOS temporary
// directories, never the Linux layout.
func TestBackfillEnvironmentDefaultsToTheRealSystem(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	env := testEnv(t, t.TempDir(), time.Now())
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	env.OS = ""
	env.BackfillTempDirs = nil
	env.LookupEnv = func(string) (string, bool) { return "", false }

	want := platform.Unknown
	switch runtime.GOOS {
	case "darwin":
		want = platform.Darwin
	case "linux":
		want = platform.Linux
	}
	if got := env.operatingSystem(); got != want {
		t.Errorf("operatingSystem() = %q, want %q", got, want)
	}
	if got, want := env.cursorDatabase(), (Env{UserHomeDir: func() (string, error) { return userHome, nil }}).cursorDatabase(); got != want {
		t.Errorf("cursorDatabase() = %q, want %q", got, want)
	}
	if runtime.GOOS == "darwin" {
		if got, want := env.cursorDatabase(), filepath.Join(userHome, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"); got != want {
			t.Errorf("on macOS cursorDatabase() = %q, want %q", got, want)
		}
	}
	bf := env.backfillEnvironment(userHome, config.Config{})
	wantTemp := []string{"/tmp", "/var/tmp"}
	if runtime.GOOS == "darwin" {
		wantTemp = []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}
	}
	if !equalStrings(bf.DefaultTempDirs(), wantTemp) || !equalStrings(bf.TempDirs, wantTemp) {
		t.Errorf("temporary directories %v (defaults %v), want %v", bf.TempDirs, bf.DefaultTempDirs(), wantTemp)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
