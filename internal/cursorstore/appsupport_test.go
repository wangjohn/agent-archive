package cursorstore

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

// TestStateDatabaseUsesTheRealSystem: StateDatabase is
// platform.Locations.CursorStateDB for this process's OS and environment (the
// layouts themselves, on every OS, are tested in internal/platform). On macOS
// the result is what it always was, byte for byte.
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

// A system with no known location for Cursor's database (platform.Locations
// answers "" there) reads as Cursor not installed, not as a database at a
// path relative to the working directory.
func TestNoLocationIsNoDatabase(t *testing.T) {
	t.Parallel()
	if _, err := resolve(""); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("resolve(\"\") = %v, want ErrNoDatabase", err)
	}
	if _, err := ReadSignature(context.Background(), "", "any"); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("ReadSignature with no location = %v, want ErrNoDatabase", err)
	}
}
