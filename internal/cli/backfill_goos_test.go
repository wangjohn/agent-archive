package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// Env.BackfillGOOS decides where backfill, and the collector, look for
// Cursor's data, and which temporary directories backfill skips. On Linux
// Cursor's database is under the XDG config home, read from the Env's
// environment (LookupEnv), not the test process's.
func TestBackfillEnvironmentFollowsBackfillGOOS(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	xdg := t.TempDir()
	for _, tc := range []struct {
		name string
		goos string
		xdg  string
		want string
		temp []string
		// tmpdir is $TMPDIR in the Env's environment; a non-blank one is
		// appended (trimmed) to the default temporary directories.
		tmpdir string
		// wantTemp is the complete list of temporary directories.
		wantTemp []string
	}{
		{"darwin", "darwin", xdg, filepath.Join(userHome, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"),
			[]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"}, "  /private/tmp/mine \n",
			[]string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders", "/private/tmp/mine"}},
		{"linux", "linux", "", filepath.Join(userHome, ".config", "Cursor", "User", "globalStorage", "state.vscdb"),
			[]string{"/tmp", "/var/tmp"}, "   ", []string{"/tmp", "/var/tmp"}},
		{"linux with XDG_CONFIG_HOME", "linux", xdg, filepath.Join(xdg, "Cursor", "User", "globalStorage", "state.vscdb"),
			[]string{"/tmp", "/var/tmp"}, "/home/me/scratch", []string{"/tmp", "/var/tmp", "/home/me/scratch"}},
	} {
		env := testEnv(t, t.TempDir(), time.Now())
		env.UserHomeDir = func() (string, error) { return userHome, nil }
		env.BackfillGOOS = tc.goos
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
		if bf.GOOS != tc.goos {
			t.Errorf("%s: GOOS %q", tc.name, bf.GOOS)
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
