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

// ModuleImports returns, for every package of the module the test runs in,
// the packages its production (non-test) files import directly, for
// the current build context. An architecture test that says "only these
// packages may import that one" scans it. Keys are import paths; each list is
// sorted.
func ModuleImports(tb testing.TB) map[string][]string {
	tb.Helper()
	all := map[string][]string{}
	module := goListLines(tb, "-m", "-f", "{{.Path}}")[0]
	for _, line := range goListLines(tb, "-f", `{{.ImportPath}} {{join .Imports " "}}`, module+"/...") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		imports := slices.Clone(fields[1:])
		slices.Sort(imports)
		all[fields[0]] = imports
	}
	return all
}

// Importers returns the sorted packages of the module whose production files
// import path directly, from a ModuleImports result.
func Importers(module map[string][]string, path string) []string {
	var found []string
	for pkg, imports := range module {
		if slices.Contains(imports, path) {
			found = append(found, pkg)
		}
	}
	slices.Sort(found)
	return found
}

func goListLines(tb testing.TB, args ...string) []string {
	tb.Helper()
	out, err := exec.CommandContext(tb.Context(), "go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		tb.Fatalf("go list %s: %v", strings.Join(args, " "), err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// FileImports checks an independently compilable production declaration boundary.
// Files from effectful siblings cannot satisfy hidden helper references.
func FileImports(tb testing.TB, files ...string) (direct, all []string) {
	tb.Helper()
	out, err := exec.CommandContext(tb.Context(), "go", append([]string{"test", "-run=^$"}, files...)...).CombinedOutput()
	if err != nil {
		tb.Fatalf("pure declaration boundary %v: %v\n%s", files, err, out)
	}
	direct = goList(tb, append([]string{"-f", `{{join .Imports "\n"}}`}, files...)...)
	seen := map[string]bool{}
	for _, p := range direct {
		for _, dep := range goList(tb, "-deps", p) {
			seen[dep] = true
		}
	}
	for p := range seen {
		all = append(all, p)
	}
	slices.Sort(direct)
	slices.Sort(all)
	return direct, all
}
