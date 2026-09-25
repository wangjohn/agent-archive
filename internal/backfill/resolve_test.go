package backfill

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// A worktree outside its repository goes through rule 2 on the repository it
// maps to: excluded with it, or imported under its configured spelling.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestWorktreeOutsideRepository(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	feature := tr.worktree("home/repo", "home/repo-feature", "repo-feature")
	if err := os.Symlink(tr.home, tr.path("alias")); err != nil {
		t.Fatal(err)
	}
	aliased := filepath.Join(tr.path("alias"), "repo")
	cases := []struct {
		name string
		cfg  config.Config
		want resolution
	}{
		{"unconfigured", config.Config{}, resolution{root: repo, kind: ProjectKindRepository}},
		{"excluded", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(repo, false)}}}, resolution{root: repo, kind: ProjectKindRepository, skip: SkipExcludedProject}},
		{"included", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(repo, true)}}}, resolution{root: repo, kind: ProjectKindRepository, included: true}},
		{"included through a symlinked spelling", config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(aliased, true)}}}, resolution{root: aliased, kind: ProjectKindRepository, included: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newResolver(tr.env(), tc.cfg, Filters{}).resolve(feature); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A worktree whose git directory is gone is not a repository of its own.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestWorktreeWithMissingRepository(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	codexWT := tr.path("home/.codex/worktrees/ab12/repo")
	tr.write("home/.codex/worktrees/ab12/repo/.git", "gitdir: "+filepath.Join(tr.root, "gone", ".git", "worktrees", "x")+"\n")
	claudeWT := tr.path("home/repo/.claude/worktrees/w1")
	tr.write("home/repo/.claude/worktrees/w1/.git", "gitdir: ../../../.git/worktrees/w1\n")
	cases := []struct {
		name string
		cwd  string
		want resolution
	}{
		{"Codex worktree", codexWT, resolution{skip: SkipWorktreeUnresolved}},
		{"Claude worktree maps by path", claudeWT, resolution{root: repo, kind: ProjectKindRepository}},
		{"missing worktree directly under home", filepath.Join(tr.home, ".claude", "worktrees", "x"), resolution{root: tr.home, kind: ProjectKindHome, skip: SkipHomeDirectory}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newResolver(tr.env(), config.Config{}, Filters{}).resolve(tc.cwd); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A folder that no longer exists under a symlinked parent resolves through
// the parent, so its spelling and configured owner match the real path.
//
// Regression: backfill review, 2026-09 (cbce179).
func TestMissingFolderUnderSymlink(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	proj := tr.mkdir("home/proj")
	if err := os.Symlink(tr.home, tr.path("alias")); err != nil {
		t.Fatal(err)
	}
	r := newResolver(tr.env(), config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(proj, true)}}}, Filters{})
	if got, want := r.resolve(filepath.Join(tr.path("alias"), "gone", "x")), (resolution{root: filepath.Join(tr.home, "gone", "x"), kind: ProjectKindDirectory}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got := r.resolve(filepath.Join(tr.path("alias"), "proj", "missing")); got.root != proj || !got.included {
		t.Fatalf("configured owner through a symlink: %+v", got)
	}
}

// A missing worktree under home maps to home by its path; when home itself
// is a configured, included project, that configuration wins.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestWorktreeMappedToConfiguredHome(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{project(tr.home, true)}}}
	got := newResolver(tr.env(), cfg, Filters{}).resolve(filepath.Join(tr.home, ".claude", "worktrees", "gone"))
	if want := (resolution{root: tr.home, kind: ProjectKindHome, included: true}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An existing worktree outside the apps' worktree folders whose git
// directory is gone cannot be mapped to its repository.
//
// Regression: backfill B2 second review, 2026-09 (eb5b4e3).
func TestExistingWorktreeWithMissingGitDir(t *testing.T) {
	t.Parallel()
	tr := newTree(t)
	wt := tr.mkdir("home/elsewhere/feature")
	tr.write("home/elsewhere/feature/.git", "gitdir: "+filepath.Join(tr.root, "gone", ".git", "worktrees", "feature")+"\n")
	if got := newResolver(tr.env(), config.Config{}, Filters{}).resolve(filepath.Join(wt, "pkg")); got != (resolution{skip: SkipWorktreeUnresolved}) {
		t.Fatalf("got %+v", got)
	}
}
