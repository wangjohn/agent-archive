package codexmeta

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

// Archive and stats may share this decoder only while it stays deterministic
// and cannot access native files, processes, environment, network or randomness.
func TestMetadataInterpretationIsPure(t *testing.T) {
	t.Parallel()
	direct, _ := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/codexmeta")
	allowed := []string{"encoding/json", "errors", "path/filepath", "regexp", "strconv", "strings", "time", "unicode"}
	for _, path := range direct {
		if !slices.Contains(allowed, path) {
			t.Errorf("codexmeta imports %s outside its pure standard-library boundary", path)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("source files: %v", err)
	}
	fset := token.NewFileSet()
	clock := []string{"Now", "Since", "Until", "After", "AfterFunc", "Tick", "NewTicker", "NewTimer", "Sleep"}
	paths := []string{"Base", "Clean", "Dir", "Ext", "FromSlash", "IsAbs", "IsLocal", "Join", "Localize", "Match", "Rel", "Split", "SplitList", "ToSlash", "VolumeName"}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		aliases := map[string]string{}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Base(path)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias == "." {
				t.Errorf("%s uses a dot import that bypasses effect checks", name)
			}
			aliases[alias] = path
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			path := aliases[pkg.Name]
			if path == "time" && slices.Contains(clock, sel.Sel.Name) || path == "path/filepath" && !slices.Contains(paths, sel.Sel.Name) {
				t.Errorf("%s: %s.%s accesses external state; interpret supplied values only", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
	}
}
