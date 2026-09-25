// Package importgraph lets a test pin a package's import boundary: which
// packages its production files import directly, and which it reaches
// through them. The architecture tests use it so a boundary holds on every
// platform `go test` runs on, alongside the depguard rules in .golangci.yml
// that CI lints on macOS.
//
// It is test code only; depguard keeps it out of production packages.
package importgraph

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Imports returns the packages the production (non-test) files of the
// package at importPath import directly, and every package they reach,
// transitively, for the current build context. Both are sorted. It asks the
// go command, which `go test` always has.
func Imports(tb testing.TB, importPath string) (direct, all []string) {
	tb.Helper()
	direct = goList(tb, "-f", `{{join .Imports "\n"}}`, importPath)
	for _, path := range goList(tb, "-deps", importPath) {
		if path != importPath {
			all = append(all, path)
		}
	}
	slices.Sort(direct)
	slices.Sort(all)
	return direct, all
}

// TestImports returns the packages the test files of the package at
// importPath import directly (its own and its external _test package's),
// sorted and without duplicates.
func TestImports(tb testing.TB, importPath string) []string {
	tb.Helper()
	imports := goList(tb, "-f", `{{join .TestImports "\n"}}{{"\n"}}{{join .XTestImports "\n"}}`, importPath)
	slices.Sort(imports)
	return slices.Compact(imports)
}

func goList(tb testing.TB, args ...string) []string {
	tb.Helper()
	out, err := exec.CommandContext(tb.Context(), "go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		tb.Fatalf("go list %s: %v", strings.Join(args, " "), err)
	}
	return strings.Fields(string(out))
}

// Forbid fails the test for each of imports that is in forbidden, naming
// what imported it.
func Forbid(tb testing.TB, what string, imports []string, forbidden ...string) {
	tb.Helper()
	for _, path := range imports {
		if slices.Contains(forbidden, path) {
			tb.Errorf("%s imports %s", what, path)
		}
	}
}
