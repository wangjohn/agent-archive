package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

const platformImportPath = "github.com/wangjohn/agent-archive/internal/platform"

// The platform package is pure: it runs no program and opens no file, and it
// is built from nothing that opens credentials or edits app hook files. What
// it needs from the system arrives through LocationDeps. depguard's
// platform-is-pure rule in .golangci.yml says the same on macOS.
func TestPlatformImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, platformImportPath)
	importgraph.Forbid(t, "internal/platform", direct,
		"os", "os/exec", "os/signal", "net", "net/http", "io/ioutil",
		"github.com/wangjohn/agent-archive/internal/credentials",
		"github.com/wangjohn/agent-archive/internal/hooks",
	)
	importgraph.Forbid(t, "internal/platform (transitively)", all,
		"os/exec", "net/http",
		"github.com/wangjohn/agent-archive/internal/credentials",
		"github.com/wangjohn/agent-archive/internal/hooks",
	)
	for _, path := range all {
		if strings.HasPrefix(path, "github.com/wangjohn/agent-archive/") {
			t.Errorf("internal/platform reaches %s; it depends on the standard library alone", path)
		}
	}
}

// runtimeGOOSUses lists where src (a Go file's text) reads runtime.GOOS,
// through whatever name it imports runtime under, or with a dot import. An
// import of go/build counts as a read too: build.Default.GOOS is the host's
// runtime.GOOS under another name, and nothing outside tests needs go/build.
func runtimeGOOSUses(t *testing.T, name string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var uses []string
	// A file may import runtime more than once, under different names.
	names := map[string]bool{}
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		if path == "go/build" && (spec.Name == nil || spec.Name.Name != "_") {
			uses = append(uses, fset.Position(spec.Pos()).String()+": imports go/build, whose build.Default.GOOS is runtime.GOOS")
		}
		if path != "runtime" {
			continue
		}
		local := "runtime"
		if spec.Name != nil {
			local = spec.Name.Name
		}
		if local == "." {
			uses = append(uses, fset.Position(spec.Pos()).String()+": dot-imports runtime, which hides runtime.GOOS")
		} else if local != "_" {
			names[local] = true
		}
	}
	if len(names) == 0 {
		return uses
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && names[pkg.Name] && sel.Sel.Name == "GOOS" {
			uses = append(uses, fset.Position(sel.Pos()).String()+": runtime.GOOS")
		}
		return true
	})
	return uses
}

// runtime.GOOS is read in one place, platform.Current, so every caller gets
// the OS as a value it can be handed in a test, and an unknown system is
// handled in one type instead of by string comparisons. Build-tagged files
// choose their code at compile time and are not affected by this.
func TestOnlyPlatformReadsRuntimeGOOS(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not two levels up: %v", err)
	}
	platformDir := filepath.Join(root, "internal", "platform")
	checked, inPlatform := 0, 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			// The go command ignores these; so do worktrees and other tools'
			// folders, which can hold whole other copies of the repository.
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); path != root && err == nil {
				return filepath.SkipDir // another module
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		uses := runtimeGOOSUses(t, path, nil)
		if filepath.Dir(path) == platformDir {
			inPlatform += len(uses)
			return nil
		}
		checked++
		for _, use := range uses {
			t.Errorf("%s; only internal/platform reads it (platform.Current)", use)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d source files were checked; the scan is not finding the repository", checked)
	}
	if inPlatform == 0 {
		t.Fatal("internal/platform no longer reads runtime.GOOS itself; is Current still the reader?")
	}
}

// The scan finds runtime.GOOS however it is spelled, and only that.
func TestRuntimeGOOSScanFindsEverySpelling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"plain", "package p\nimport \"runtime\"\nvar x = runtime.GOOS\n", 1},
		{"twice", "package p\nimport \"runtime\"\nvar x, y = runtime.GOOS, runtime.GOOS\n", 2},
		{"renamed import", "package p\nimport rt \"runtime\"\nvar x = rt.GOOS\n", 1},
		{"dot import", "package p\nimport . \"runtime\"\nvar x = GOOS\n", 1},
		{"imported twice, read through the first name", "package p\nimport (\n\trt \"runtime\"\n\t\"runtime\"\n)\nvar x = rt.GOOS\nvar n = runtime.NumCPU()\n", 1},
		{"imported twice, read through both names", "package p\nimport (\n\trt \"runtime\"\n\t\"runtime\"\n)\nvar x, y = rt.GOOS, runtime.GOOS\n", 2},
		{"blank import", "package p\nimport _ \"runtime\"\nvar runtime struct{ GOOS string }\nvar x = runtime.GOOS\n", 0},
		{"other runtime names", "package p\nimport \"runtime\"\nvar x = runtime.GOARCH\nvar n = runtime.NumCPU()\n", 0},
		{"another package's GOOS", "package p\nimport \"other\"\nvar x = other.GOOS\n", 0},
		{"a local variable named runtime", "package p\nvar runtime struct{ GOOS string }\nvar x = runtime.GOOS\n", 0},
		{"no imports", "package p\nvar x = 1\n", 0},
		{"go/build", "package p\nimport \"go/build\"\nvar x = build.Default.GOOS\n", 1},
		{"go/build renamed, context copied", "package p\nimport gb \"go/build\"\nvar ctx = gb.Default\nvar x = ctx.GOOS\n", 1},
		{"go/build blank import", "package p\nimport _ \"go/build\"\n", 0},
		{"cgo file", "package p\n\n// #include <stdlib.h>\nimport \"C\"\nimport \"runtime\"\nvar x = runtime.GOOS\n", 1},
	} {
		if got := runtimeGOOSUses(t, tc.name+".go", tc.src); len(got) != tc.want {
			t.Errorf("%s: %d uses %v, want %d", tc.name, len(got), got, tc.want)
		}
	}
}
