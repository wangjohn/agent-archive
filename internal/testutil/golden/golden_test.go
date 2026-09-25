package golden

import (
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
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
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
