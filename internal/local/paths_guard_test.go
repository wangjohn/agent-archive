package local

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A containment check written as a string prefix test against a path
// separator is another copy of PathWithin; each copy got the edges wrong in
// its own way. The ones listed are not containment checks, or wait on
// another change.
func TestNoOtherPathContainmentCheck(t *testing.T) {
	allowed := map[string]string{
		"internal/local/paths.go": "PathWithin itself",
		// display cuts the home prefix off to print ~/rest; it needs the
		// remainder, not a yes or no.
		"internal/backfill/render.go": "prints the remainder",
		// TODO(#55): sameProject; switch to PathWithin once that PR, which
		// edits handoff.go, has merged.
		"internal/cli/handoff.go": "pending #55",
	}
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || fn.Sel.Name != "HasPrefix" {
				return true
			}
			if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "strings" {
				return true
			}
			prefix := string(src[fset.Position(call.Args[1].Pos()).Offset:fset.Position(call.Args[1].End()).Offset])
			if strings.Contains(prefix, "Separator") {
				if _, ok := allowed[rel]; !ok {
					t.Errorf("%s: strings.HasPrefix(..., %s) checks path containment; use local.PathWithin", fset.Position(call.Pos()), prefix)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
