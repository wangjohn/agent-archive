package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// scopeStub is scopeDependencies with a fixed working directory and one
// repository key per directory, so no test runs git.
type scopeStub struct {
	home string
	dir  string
	keys map[string]string
	asks int
}

func (s *scopeStub) workingDir() (string, error) {
	if s.dir == "" {
		return "", errors.New("no working directory")
	}
	return s.dir, nil
}

func (s *scopeStub) readHome() (string, error) { return s.home, nil }

func (s *scopeStub) repoKeyResolver() func(string) string {
	return func(root string) string {
		s.asks++
		return s.keys[root]
	}
}

// scopeHome saves a configuration holding the given project roots.
func scopeHome(t *testing.T, roots ...string) string {
	t.Helper()
	home := t.TempDir()
	cfg := config.Config{MachineID: "machine-1", Storage: credentialsTestConfig(), Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1", Enabled: true}}
	for _, root := range roots {
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: time.Unix(0, 0)})
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	return home
}

const (
	scopeKey      = "repo-0123456789abcdef"
	otherScopeKey = "repo-fedcba9876543210"
)

// Two worktrees of one repository are one scope, whichever the command runs
// in, and a session of another repository is in neither.
func TestScopeOfTwoWorktreesOfOneRepositoryIsOneScope(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	main, worktree := filepath.Join(root, "agent-archive"), filepath.Join(root, "agent-archive-pr4")
	stub := &scopeStub{home: scopeHome(t, main), keys: map[string]string{main: scopeKey, worktree: scopeKey}}
	inMain := archive.Metadata{ProjectID: archive.ProjectID(main), ProjectName: "agent-archive", RepoKey: scopeKey}
	inWorktree := archive.Metadata{ProjectID: archive.ProjectID(worktree), ProjectName: "agent-archive-pr4", RepoKey: scopeKey}
	elsewhere := archive.Metadata{ProjectID: archive.ProjectID(main), ProjectName: "agent-archive", RepoKey: otherScopeKey}
	for _, dir := range []string{main, worktree} {
		stub.dir = dir
		scope, err := scopeFor(stub, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if scope.RepoKey != scopeKey || !scope.narrowed() {
			t.Fatalf("%s: scope %+v", dir, scope)
		}
		if !scope.contains(inMain, nil) || !scope.contains(inWorktree, nil) {
			t.Errorf("%s: a session of the repository is out of scope", dir)
		}
		// The key decides: the checkout's path does not bring in a
		// session of another repository.
		if scope.contains(elsewhere, nil) {
			t.Errorf("%s: a session of another repository is in scope", dir)
		}
		// A registration on this Mac carries the key too.
		reg := archive.SessionRegistration{ProjectRoot: worktree, RepoKey: scopeKey}
		if !scope.contains(archive.Metadata{}, &reg) {
			t.Errorf("%s: a registered session of the repository is out of scope", dir)
		}
	}
	// The repository key is asked once per scope.
	stub.asks = 0
	if _, err := scopeFor(stub, "", false); err != nil || stub.asks != 1 {
		t.Fatalf("asked for the repository key %d times, err=%v", stub.asks, err)
	}
}

// With no origin remote on either side, a local session is in scope when its
// project root holds the directory, and an archived one when its project ID
// is one the directory can have, as --latest decides.
func TestScopeWithNoOriginFallsBackToSameProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	project, other := filepath.Join(root, "app"), filepath.Join(root, "other")
	stub := &scopeStub{home: scopeHome(t, project), dir: filepath.Join(project, "internal", "pkg"), keys: map[string]string{}}
	scope, err := scopeFor(stub, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if scope.RepoKey != "" || scope.Label != "app" || !scope.narrowed() {
		t.Fatalf("scope %+v", scope)
	}
	local := func(root string) *archive.SessionRegistration {
		return &archive.SessionRegistration{ProjectRoot: root, ProjectID: archive.ProjectID(root)}
	}
	if !scope.contains(archive.Metadata{}, local(project)) || scope.contains(archive.Metadata{}, local(other)) {
		t.Error("a local session is not in scope by its project root")
	}
	if !scope.contains(archive.Metadata{ProjectID: archive.ProjectID(project)}, nil) || scope.contains(archive.Metadata{ProjectID: archive.ProjectID(other)}, nil) {
		t.Error("an archived session is not in scope by its project ID")
	}
	// An older session with no key, and a scope with one, still go by path.
	stub.keys[stub.dir] = scopeKey
	keyed, err := scopeFor(stub, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !keyed.contains(archive.Metadata{ProjectID: archive.ProjectID(project)}, nil) {
		t.Error("an archived session with no key is not in scope by its project ID")
	}
	if keyed.contains(archive.Metadata{ProjectID: archive.ProjectID(other)}, nil) {
		t.Error("an archived session of another project is in a keyed scope")
	}
}

// Outside any project there is no scope: every session is in it.
func TestScopeOutsideAnyProjectHoldsEverySession(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stub := &scopeStub{home: scopeHome(t, filepath.Join(root, "app")), dir: filepath.Join(root, "notes"), keys: map[string]string{}}
	for _, dir := range []string{stub.dir, ""} {
		stub.dir = dir
		scope, err := scopeFor(stub, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if scope.Label != "" || !scope.All || scope.narrowed() {
			t.Fatalf("%q: scope %+v", dir, scope)
		}
		if !scope.contains(archive.Metadata{ProjectID: "anything", RepoKey: scopeKey}, nil) {
			t.Errorf("%q: a session is out of no scope", dir)
		}
	}
}

// --all-projects keeps the scope's name and key, so a browser can offer to go
// back to it, and turns it off.
func TestScopeAllProjectsKeepsTheLabel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	app := filepath.Join(root, "app")
	stub := &scopeStub{home: scopeHome(t, app), dir: app, keys: map[string]string{app: scopeKey}}
	scope, err := scopeFor(stub, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Label != "app" || scope.RepoKey != scopeKey || !scope.All || scope.narrowed() {
		t.Fatalf("scope %+v", scope)
	}
	if !scope.contains(archive.Metadata{RepoKey: otherScopeKey}, nil) || scope.only().contains(archive.Metadata{RepoKey: otherScopeKey}, nil) {
		t.Error("All does not switch the scope off, or only() does not switch it on")
	}
}

// --project takes a directory, which gives that directory's scope, or a
// project name, matched exactly and ignoring case against project_name and
// the configured project labels.
func TestScopeProjectArgumentIsADirectoryOrAName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	app, billing := filepath.Join(root, "app"), filepath.Join(root, "Billing")
	for _, dir := range []string{app, billing} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The working directory is elsewhere: --project overrides it.
	stub := &scopeStub{home: scopeHome(t, app, billing), dir: filepath.Join(root, "notes"), keys: map[string]string{billing: otherScopeKey}}
	byDir, err := scopeFor(stub, billing, false)
	if err != nil {
		t.Fatal(err)
	}
	if byDir.Label != "Billing" || byDir.RepoKey != otherScopeKey || byDir.Dir != billing {
		t.Fatalf("by directory: %+v", byDir)
	}
	byName, err := scopeFor(stub, "billing", false)
	if err != nil {
		t.Fatal(err)
	}
	if byName.Dir != "" || byName.Label != "billing" || !byName.narrowed() {
		t.Fatalf("by name: %+v", byName)
	}
	for name, want := range map[string]bool{"billing": true, "BILLING": true, "Billing": true, "bill": false, "billing-api": false, "app": false} {
		scope, err := scopeFor(stub, name, false)
		if err != nil {
			t.Fatal(err)
		}
		// A name is matched to project_name...
		if got := scope.contains(archive.Metadata{ProjectName: "Billing"}, nil); got != want {
			t.Errorf("--project %q: project_name Billing in scope = %v, want %v", name, got, want)
		}
		// ...and to a configured project's label, by its project ID.
		if got := scope.contains(archive.Metadata{ProjectID: archive.ProjectID(billing)}, nil); got != want {
			t.Errorf("--project %q: the configured project Billing in scope = %v, want %v", name, got, want)
		}
	}
	// A registration on this Mac names its project by its root.
	reg := archive.SessionRegistration{ProjectRoot: billing}
	if !byName.contains(archive.Metadata{}, &reg) {
		t.Error("a registered session of project Billing is out of scope")
	}
}

// A name that matches no project is still a scope, which holds nothing: the
// browser then falls back to all projects and says so.
func TestScopeNameThatMatchesNothingIsStillAScope(t *testing.T) {
	t.Parallel()
	stub := &scopeStub{home: scopeHome(t), dir: t.TempDir(), keys: map[string]string{}}
	scope, err := scopeFor(stub, "no-such-project", false)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.narrowed() || scope.contains(archive.Metadata{ProjectName: "app"}, nil) {
		t.Fatalf("scope %+v", scope)
	}
	if strings.Contains(scope.Label, "/") {
		t.Fatalf("label %q", scope.Label)
	}
}

// gitCheckout makes dir a git checkout's folder: a .git directory, as git
// init leaves it.
func gitCheckout(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// gitWorktree makes dir a worktree of the checkout at main, as git worktree
// add leaves it: a .git file pointing into main's .git/worktrees.
func gitWorktree(t *testing.T, main, dir string) string {
	t.Helper()
	gitDir := filepath.Join(main, ".git", "worktrees", filepath.Base(dir))
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A scope made from a directory is named after its repository's main
// checkout, from the checkout itself, a subdirectory, or a worktree, so
// every command names it alike whatever sessions it reads.
func TestScopeIsNamedAfterTheRepositorysMainCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	main := gitCheckout(t, filepath.Join(root, "agent-archive"))
	sub := filepath.Join(main, "internal", "cli")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := gitWorktree(t, main, filepath.Join(root, "wt", "pr4"))
	worktreeSub := filepath.Join(worktree, "docs")
	if err := os.MkdirAll(worktreeSub, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := &scopeStub{home: scopeHome(t), keys: map[string]string{}}
	for _, dir := range []string{main, sub, worktree, worktreeSub} {
		stub.keys[dir] = scopeKey
		stub.dir = dir
		scope, err := scopeFor(stub, "", false)
		if err != nil {
			t.Fatal(err)
		}
		if scope.Label != "agent-archive" {
			t.Errorf("%s: label %q, want agent-archive", dir, scope.Label)
		}
		// --project names a worktree's scope the same way.
		if scope, err = scopeFor(stub, dir, true); err != nil || scope.Label != "agent-archive" {
			t.Errorf("--project %s: label %q, err %v", dir, scope.Label, err)
		}
	}
}

// A worktree is named after the checkout its .git file links to; a
// submodule or any other link is its own repository. A checkout reached
// through a symbolic link is named after the folder it links to.
func TestRepositoryNameOfEachKindOfCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(dir, gitFile string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(gitFile), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	main := gitCheckout(t, filepath.Join(root, "app"))
	bare := filepath.Join(root, "service.git")
	if err := os.MkdirAll(filepath.Join(bare, "worktrees", "feature"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A link of another name to the main checkout, as its worktrees never
	// name it.
	alias := filepath.Join(root, "alias")
	if err := os.MkdirAll(filepath.Join(main, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(main, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		dir  string
		want string
		ok   bool
	}{
		{"a main checkout", main, "app", true},
		{"a main checkout through a link", alias, "app", true},
		{"a subdirectory through a link", filepath.Join(alias, "sub"), "app", true},
		{"a worktree", gitWorktree(t, main, filepath.Join(root, "app-pr7")), "app", true},
		{"a worktree linked by a relative path", write(filepath.Join(root, "rel"), "gitdir: ../app/.git/worktrees/rel\n"), "app", true},
		{"a worktree of a bare repository", write(filepath.Join(root, "feature"), "gitdir: "+filepath.Join(bare, "worktrees", "feature")), "service", true},
		{"a submodule", write(filepath.Join(main, "vendor", "lib"), "gitdir: ../../.git/modules/lib\n"), "lib", true},
		{"an unreadable link", write(filepath.Join(root, "odd"), "not a link"), "odd", true},
		{"no checkout", root, "", false},
	} {
		got, ok := repositoryName(tc.dir)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: repositoryName = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
