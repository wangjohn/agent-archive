package scheduler

import (
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

const (
	modulePath       = "github.com/wangjohn/agent-archive/"
	schedulerPath    = modulePath + "internal/scheduler"
	launchdPath      = modulePath + "internal/scheduler/launchd"
	hostPath         = modulePath + "internal/scheduler/host"
	credentialsPath  = modulePath + "internal/credentials"
	hooksPath        = modulePath + "internal/hooks"
	adapterDirPrefix = schedulerPath + "/"
)

// The port is pure: it runs no program (an adapter runs its tool through the
// Runner it is given), and it is built from nothing that opens credentials or
// edits app hook files. depguard's scheduler-is-pure rule in .golangci.yml
// says the same on macOS.
func TestSchedulerImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, schedulerPath)
	importgraph.Forbid(t, "internal/scheduler", direct, "os/exec", "os/signal", "net", "net/http", credentialsPath, hooksPath)
	importgraph.Forbid(t, "internal/scheduler (transitively)", all, "os/exec", "net/http", credentialsPath, hooksPath)
	for _, path := range all {
		if strings.HasPrefix(path, modulePath) {
			t.Errorf("internal/scheduler reaches %s; the port depends on the standard library alone", path)
		}
	}
}

// adapterImporters are the packages that may import an adapter
// (internal/scheduler/launchd, and the ones after it): host, which chooses
// one for the system, and cli, for the launchd vocabulary the setup flows
// still speak (plist labels, the plist codec, its paths). The second entry
// goes when 5a-3 of dev/proposals/platform-abstraction.md moves that history
// behind the adapter; a package that no longer imports one fails the test, so
// the list only shrinks.
var adapterImporters = []string{
	modulePath + "internal/cli",
	hostPath,
}

// Only host imports the adapters, and cli for the launchd vocabulary it has
// not yet moved behind the port. An adapter imports no other adapter.
func TestOnlyHostImportsAdapters(t *testing.T) {
	t.Parallel()
	module := importgraph.ModuleImports(t)
	adapters := adapterPackages(module)
	if !slices.Contains(adapters, launchdPath) {
		t.Fatalf("the adapter scan found %v, not the launchd adapter", adapters)
	}
	seen := map[string]bool{}
	for _, adapter := range adapters {
		for _, importer := range importgraph.Importers(module, adapter) {
			seen[importer] = true
			if !slices.Contains(adapterImporters, importer) && !slices.Contains(adapters, importer) {
				t.Errorf("%s imports the adapter %s; only host does (and cli, for now)", importer, adapter)
			}
			if slices.Contains(adapters, importer) {
				t.Errorf("the adapter %s imports the adapter %s", importer, adapter)
			}
		}
	}
	for _, allowed := range adapterImporters {
		if !seen[allowed] {
			t.Errorf("%s is allowed to import an adapter and no longer does; remove it from adapterImporters", allowed)
		}
	}
}

// adapterPackages are the packages under internal/scheduler other than host:
// the adapters.
func adapterPackages(module map[string][]string) []string {
	var found []string
	for pkg := range module {
		if strings.HasPrefix(pkg, adapterDirPrefix) && pkg != hostPath {
			found = append(found, pkg)
		}
	}
	slices.Sort(found)
	return found
}

// osExecImporters are the packages whose production files may run a program.
// An adapter is not among them: it runs its tool through the Runner it is
// given, and host owns the real one. A package joins the list deliberately,
// with a reason:
//
//   - cli: pager, capabilities (`claude --version`), and handoff's git and
//     terminal launching
//   - cursorstore: getconf, for macOS's per-user temporary directory
//   - scheduler/host: the default Runner
//   - termlaunch: opens a terminal for handoff
//   - testutil/importgraph and testutil/isolation: test helpers
//
// depguard's rule in .golangci.yml is the mirror of this list.
var osExecImporters = []string{
	modulePath + "internal/cli",
	modulePath + "internal/cursorstore",
	hostPath,
	modulePath + "internal/termlaunch",
	modulePath + "internal/testutil/importgraph",
	modulePath + "internal/testutil/isolation",
}

// Running a program is limited to a listed set of packages, so a new one
// (a second scheduler adapter, say) cannot start shelling out unnoticed.
func TestOnlyListedPackagesRunPrograms(t *testing.T) {
	t.Parallel()
	got := importgraph.Importers(importgraph.ModuleImports(t), "os/exec")
	for _, pkg := range got {
		if !slices.Contains(osExecImporters, pkg) {
			t.Errorf("%s imports os/exec; add it to osExecImporters (and depguard's rule) with a reason, or run the program through a scheduler.Runner", pkg)
		}
	}
	for _, pkg := range osExecImporters {
		if !slices.Contains(got, pkg) {
			t.Errorf("%s is listed as an os/exec importer and is not one; remove it from osExecImporters and depguard's rule", pkg)
		}
	}
}
