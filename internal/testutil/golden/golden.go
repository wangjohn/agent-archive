// Package golden is how this repository's tests compare output with a
// golden file, and the one place the -update flag that rewrites golden files
// is defined:
//
//	go test ./... -update
//
// rewrites every golden file in every package. Go passes -update to each
// package's test binary, and a binary that does not define it fails, so every
// package with tests imports this one (TestEveryTestedPackageKnowsUpdate
// checks); a package without golden files imports it blank. Review the diff
// an update makes: a golden change is a behavior change, and for the
// archive's filter goldens a privacy change.
//
// It is test code only; depguard keeps it out of production packages.
package golden

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files from the current output")

// Update reports whether this run rewrites golden files (go test -update).
func Update() bool { return *update }

// Write writes got to the golden file at path, creating its directory.
func Write(t testing.TB, path string, got []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, got, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Check fails t unless got equals the golden file at path. With -update it
// first writes got there, so the run passes and the change shows in the
// diff.
func Check(t testing.TB, path string, got []byte) {
	t.Helper()
	if Update() {
		Write(t, path, got)
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output differs from %s (if the change is intended, rerun with -update and review the diff):\n%s", path, got)
	}
}
