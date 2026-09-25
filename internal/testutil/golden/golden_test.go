package golden

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const importPath = "github.com/wangjohn/agent-archive/internal/testutil/golden"

// Every package with tests imports this one, so `go test ./... -update`
// never fails with "flag provided but not defined" in a package that has no
// golden files of its own.
func TestEveryTestedPackageKnowsUpdate(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}
	tested := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "docs") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		if _, seen := tested[dir]; !seen {
			tested[dir] = false
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			return err
		}
		// Only a file every platform builds counts: a blank import in a
		// darwin-only file leaves the package without -update elsewhere.
		if constrained(filepath.Base(path), file) {
			return nil
		}
		for _, spec := range file.Imports {
			if p, _ := strconv.Unquote(spec.Path.Value); p == importPath {
				tested[dir] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tested) < 2 {
		t.Fatalf("found only %d tested packages under %s", len(tested), root)
	}
	here, _ := os.Getwd()
	for dir, imports := range tested {
		if !imports && dir != here {
			rel, _ := filepath.Rel(root, dir)
			t.Errorf("%s has tests but does not import %s: add\n\t_ %q // registers -update for go test ./... -update\nto one of its test files", rel, importPath, importPath)
		}
	}
}

// constrained reports whether name or a //go:build line limits the file to
// some platforms.
func constrained(name string, file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			break
		}
		for _, c := range group.List {
			if constraint.IsGoBuild(c.Text) {
				return true
			}
		}
	}
	parts := strings.Split(strings.TrimSuffix(name, "_test.go"), "_")
	for _, part := range parts[1:] {
		if knownOS[part] || knownArch[part] {
			return true
		}
	}
	return false
}

var (
	knownOS   = map[string]bool{"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "illumos": true, "ios": true, "js": true, "linux": true, "netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true}
	knownArch = map[string]bool{"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true, "mips": true, "mips64": true, "mips64le": true, "mipsle": true, "ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true}
)

func TestConstrained(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		src  string
		want bool
	}{
		{"a_test.go", "package a\n", false},
		{"keychain_isolation_test.go", "package a\n", false},
		{"a_darwin_test.go", "package a\n", true},
		{"a_linux_arm64_test.go", "package a\n", true},
		{"a_test.go", "//go:build darwin && cgo\n\npackage a\n", true},
		{"a_test.go", "// Package a is\n// go:build is only mentioned.\npackage a\n", false},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), c.name, c.src, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if got := constrained(c.name, file); got != c.want {
			t.Errorf("constrained(%s, %q) = %v, want %v", c.name, c.src, got, c.want)
		}
	}
}
