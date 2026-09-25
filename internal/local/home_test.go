package local

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Home creates AGENT_ARCHIVE_HOME, parents included, private to the account
// (0700), and tightens an existing directory that is too open. ReadHome
// resolves the same path without creating or changing anything.
func TestHomeCreatesThePrivateDataDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "nested", "agent-archive")
	t.Setenv("AGENT_ARCHIVE_HOME", want)

	read, err := ReadHome()
	if err != nil {
		t.Fatal(err)
	}
	if read != want {
		t.Fatalf("ReadHome = %q, want %q", read, want)
	}
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadHome created or touched the directory: %v", err)
	}

	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Home = %q, want %q", got, want)
	}
	assertDirMode(t, want, 0o700)

	if err := os.Chmod(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHome(); err != nil {
		t.Fatal(err)
	}
	assertDirMode(t, want, 0o755) // ReadHome changes nothing
	if _, err := Home(); err != nil {
		t.Fatal(err)
	}
	assertDirMode(t, want, 0o700)
}

// A symlinked AGENT_ARCHIVE_HOME resolves to where the data really lives.
func TestHomeResolvesASymlinkedDataDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_ARCHIVE_HOME", filepath.Join(root, "link", "data"))
	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(target, "data"); got != want {
		t.Fatalf("Home = %q, want the resolved %q", got, want)
	}
	assertDirMode(t, got, 0o700)
}

func assertDirMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != want {
		t.Fatalf("%s mode = %v, want a directory with %v", path, info.Mode(), want)
	}
}
