package capture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

const captureImportPath = "github.com/wangjohn/agent-archive/internal/capture"

// forbiddenForCapture is what neither capture nor its tests may import
// directly: a way to run a program (launchctl), handle process signals, use
// the network, touch a bucket, publish, or install hooks.
var forbiddenForCapture = []string{
	"os/exec",
	"os/signal",
	"net",
	"net/http",
	"github.com/wangjohn/agent-archive/internal/storage",
	"github.com/wangjohn/agent-archive/internal/storage/storagetest",
	"github.com/wangjohn/agent-archive/internal/collector",
	"github.com/wangjohn/agent-archive/internal/hooks",
}

// The hook runtime's import boundary, which .golangci.yml's depguard rules
// also enforce on macOS. It runs on the user's turn, so it must stay free of
// the command-line layer (flags, prompts, terminal output: internal/cli owns
// those and the exit code), and must not reach anything a hook must never do:
// run launchctl or any other program, open the Keychain or a bucket, or use
// the network. The transitive check covers what no import may bring in; the
// direct one covers what capture must not call itself (config, for one,
// imports storage for its types).
func TestCaptureImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, captureImportPath)
	importgraph.Forbid(t, "internal/capture (transitively)", all,
		"github.com/wangjohn/agent-archive/internal/cli",
		"github.com/wangjohn/agent-archive/internal/terminal",
		"golang.org/x/term",
	)
	importgraph.Forbid(t, "internal/capture", direct,
		append(slices.Clone(forbiddenForCapture), "github.com/wangjohn/agent-archive/internal/credentials")...)
}

// capture's TestMain isolates only the environment (HOME, TMPDIR, the data
// and app directory variables); unlike internal/cli it has no launchctl or
// Keychain stand-ins, because its production code cannot reach either. So
// its tests must not reach them on their own: they import nothing that runs
// a program or uses the network, and use internal/credentials only for the
// storage settings a config.Config holds, never a KeychainStore.
func TestCaptureTestsCannotReachTheMac(t *testing.T) {
	t.Parallel()
	importgraph.Forbid(t, "internal/capture's tests", importgraph.TestImports(t, captureImportPath),
		append(slices.Clone(forbiddenForCapture),
			"github.com/wangjohn/agent-archive/internal/cli",
			"github.com/wangjohn/agent-archive/internal/terminal",
			"golang.org/x/term",
		)...)

	allowed := []string{"Config", "ProviderS3", "ProviderR2"}
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files found: %v", err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		imported := false
		for _, spec := range file.Imports {
			if spec.Path.Value != `"github.com/wangjohn/agent-archive/internal/credentials"` {
				continue
			}
			imported = true
			// Renamed, dot, or blank imports would hide the uses checked below.
			if spec.Name != nil {
				t.Errorf("%s imports internal/credentials as %s; import it under its own name", name, spec.Name.Name)
			}
		}
		if !imported {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "credentials" && !slices.Contains(allowed, sel.Sel.Name) {
				t.Errorf("%s: a capture test uses credentials.%s; only %v (config.Config's storage settings) may be used here", fset.Position(sel.Pos()), sel.Sel.Name, allowed)
			}
			return true
		})
	}
}
