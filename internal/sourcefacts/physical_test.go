package sourcefacts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPhysicalProjectSeparatesNestedRepositoriesAndValidatesWorktrees(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	checkout := filepath.Join(root, "checkout")
	gd := filepath.Join(main, ".git", "worktrees", "one")
	for _, path := range []string{gd, checkout, filepath.Join(main, "nested", ".git")} {
		if e := os.MkdirAll(path, 0700); e != nil {
			t.Fatal(e)
		}
	}
	for path, body := range map[string]string{filepath.Join(checkout, ".git"): "gitdir: " + gd, filepath.Join(gd, "commondir"): "../..", filepath.Join(gd, "gitdir"): filepath.Join(checkout, ".git")} {
		if e := os.WriteFile(path, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	canonical, _ := filepath.EvalSymlinks(main)
	f, ok := PhysicalProject(checkout)
	if !ok || f.Root != canonical || f.Cwd == f.Root {
		t.Fatalf("worktree facts %#v %t", f, ok)
	}
	nested := filepath.Join(main, "nested")
	f, ok = PhysicalProject(nested)
	if !ok || filepath.Base(f.Root) != "nested" {
		t.Fatalf("nested identity %#v %t", f, ok)
	}
	_ = os.WriteFile(filepath.Join(gd, "gitdir"), []byte("/unrelated/.git"), 0600)
	if _, ok = PhysicalProject(checkout); ok {
		t.Fatal("invalid reverse mapping accepted")
	}
}

func TestProjectResolverBudgetAndCacheExpireBetweenPasses(t *testing.T) {
	root := t.TempDir()
	resolver := NewProjectResolver()
	first, ok := resolver.Resolve(root)
	if !ok {
		t.Fatal("no project")
	}
	ops := resolver.Operations
	_, ok = resolver.Resolve(root)
	if !ok || resolver.Operations <= ops || resolver.Exhausted {
		t.Fatal("same pass did not validate cached facts")
	}
	if e := os.Mkdir(filepath.Join(root, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	current, ok := NewProjectResolver().Resolve(root)
	if !ok || current.Root != first.Root {
		t.Fatal("new pass facts invalid")
	}
	for i := range 2000 {
		_, _ = resolver.Resolve(filepath.Join(root, "missing", string(rune('a'+i))))
	}
	if !resolver.Exhausted || resolver.Operations != 1024 {
		t.Fatalf("operations %d exhausted %t", resolver.Operations, resolver.Exhausted)
	}
}

func TestProjectResolverInvalidatesWorktreeAndAncestorMetadataWithinPass(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "cwd")
	if e := os.Mkdir(cwd, 0700); e != nil {
		t.Fatal(e)
	}
	resolver := NewProjectResolver()
	before, ok := resolver.Resolve(cwd)
	if !ok {
		t.Fatal("initial cwd unavailable")
	}
	if e := os.Mkdir(filepath.Join(base, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	after, ok := resolver.Resolve(cwd)
	canonical, _ := filepath.EvalSymlinks(base)
	if !ok || after.Root != canonical || before.Root == after.Root {
		t.Fatalf("stale ancestor cache before=%#v after=%#v", before, after)
	}
}

func TestPhysicalWorktreeBacklinkUsesCanonicalDirectoryAlias(t *testing.T) {
	base := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(alias, "main")
	checkout := filepath.Join(alias, "checkout")
	gitdir := filepath.Join(main, ".git", "worktrees", "one")
	for _, dir := range []string{gitdir, checkout} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{filepath.Join(checkout, ".git"): "gitdir: " + gitdir, filepath.Join(gitdir, "commondir"): "../..", filepath.Join(gitdir, "gitdir"): filepath.Join(checkout, ".git")} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	facts, ok := PhysicalProject(checkout)
	canonicalMain, err := filepath.EvalSymlinks(main)
	if err != nil {
		t.Fatal(err)
	}
	canonicalCwd, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || facts.Root != canonicalMain || facts.Cwd != canonicalCwd {
		t.Fatalf("alias changed physical worktree identity: %#v %t", facts, ok)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "gitdir"), []byte(filepath.Join(main, ".git")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := PhysicalProject(checkout); ok {
		t.Fatal("canonicalization admitted an unrelated backlink")
	}
}
