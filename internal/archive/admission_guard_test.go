package archive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// boundaryComparisons returns the position of every comparison in src that
// puts SessionStartedAt against ActivatedAt or DestinationSince: a time
// method call (Before, After, Equal, Compare, Sub) or an operator whose
// operands mention both. It sees direct uses only, not a start copied into a
// variable first; review has to catch those.
func boundaryComparisons(t *testing.T, fset *token.FileSet, file *ast.File) []token.Position {
	t.Helper()
	mentions := func(node ast.Node, names ...string) bool {
		found := false
		ast.Inspect(node, func(n ast.Node) bool {
			if selector, ok := n.(*ast.SelectorExpr); ok {
				for _, name := range names {
					if selector.Sel.Name == name {
						found = true
					}
				}
			}
			return !found
		})
		return found
	}
	var out []token.Position
	ast.Inspect(file, func(n ast.Node) bool {
		var sides []ast.Node
		switch expr := n.(type) {
		case *ast.CallExpr:
			selector, ok := expr.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "Before", "After", "Equal", "Compare", "Sub":
			default:
				return true
			}
			sides = append(sides, selector.X)
			for _, arg := range expr.Args {
				sides = append(sides, arg)
			}
		case *ast.BinaryExpr:
			// && and || join comparisons; Inspect reaches each one.
			if expr.Op == token.LAND || expr.Op == token.LOR {
				return true
			}
			sides = []ast.Node{expr.X, expr.Y}
		default:
			return true
		}
		start, boundary := false, false
		for _, side := range sides {
			start = start || mentions(side, "SessionStartedAt")
			boundary = boundary || mentions(side, "ActivatedAt", "DestinationSince")
		}
		if start && boundary {
			out = append(out, fset.Position(n.Pos()))
		}
		return true
	})
	return out
}

// Project activation and the storage destination are admission boundaries.
// A comparison of SessionStartedAt with either one outside Admitted() fails
// silently for imports, whose start is long before both: the collector skips
// the session, or retention leaves its objects behind. Compare Admitted().
func TestNoBoundaryComparesSessionStartedAtOutsideAdmitted(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, position := range boundaryComparisons(t, fset, file) {
			t.Errorf("%s: compares SessionStartedAt with an admission boundary; use SessionRegistration.Admitted()", position)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The guard must recognise the comparisons it exists to forbid.
func TestBoundaryGuardDetectsStartComparisons(t *testing.T) {
	src := `package p
func a() {
	_ = r.SessionStartedAt.Before(c.DestinationSince)
	_ = p.ActivatedAt.IsZero() || !r.SessionStartedAt.Before(p.ActivatedAt)
	_ = now.Sub(r.SessionStartedAt) > 0 && r.SessionStartedAt == c.DestinationSince
	_ = r.Admitted().Before(c.DestinationSince)
	_ = r.SessionStartedAt.Before(parent.SessionStartedAt)
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "guard.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, position := range boundaryComparisons(t, fset, file) {
		lines = append(lines, position.Line)
	}
	if len(lines) != 3 || lines[0] != 3 || lines[1] != 4 || lines[2] != 5 {
		t.Fatalf("flagged lines %v, want [3 4 5]", lines)
	}
}
