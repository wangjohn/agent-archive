package cursorstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// StateDatabase is a hermetic SQLite fixture path, independent of native locations.
func StateDatabase(home string) string { return filepath.Join(home, "synthetic", "state.vscdb") }

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
