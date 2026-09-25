package archive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCanonicalHarness(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in    string
		want  string
		known bool
	}{
		{"claude", "claude", true},
		{"Claude", "claude", true},
		{" CLAUDE ", "claude", true},
		{"claude-code", "claude", true},
		{"Claude-Code", "claude", true},
		{"codex", "codex", true},
		{"Codex\n", "codex", true},
		{"cursor", "cursor", true},
		{"CURSOR", "cursor", true},
		{"", "", false},
		{"gemini", "gemini", false},
		{"Claude Code", "claude code", false},
		{"claude_code", "claude_code", false},
	} {
		if got := CanonicalHarness(tc.in); got != tc.want {
			t.Errorf("CanonicalHarness(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got, known := KnownHarness(tc.in); got != tc.want || known != tc.known {
			t.Errorf("KnownHarness(%q) = %q, %v, want %q, %v", tc.in, got, known, tc.want, tc.known)
		}
	}
}

// Regression: 2026-09 review M-21. There were three copies of harness-name
// canonicalization, and they disagreed (backfill's rejected "Claude"). The
// alias "claude-code" may be spelled in production code only by the one
// helper, or where it is not a harness name at all; anything else is
// another copy.
func TestHarnessAliasIsSpelledOnce(t *testing.T) {
	t.Parallel()
	allowed := map[string]string{
		"internal/archive/harness.go": "the canonical helper",
		// A folder of Claude's desktop app, not a harness name.
		"internal/cli/capabilities.go": "a directory name",
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
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if value, err := strconv.Unquote(lit.Value); err == nil && strings.EqualFold(value, "claude-code") {
				if _, ok := allowed[rel]; !ok {
					t.Errorf("%s: spells the harness alias %q; call archive.CanonicalHarness instead", fset.Position(lit.Pos()), value)
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
