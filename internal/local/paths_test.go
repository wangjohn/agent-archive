package local

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: 2026-09 review M-21. Three copies of the containment check
// disagreed at the edges: the prefix-based ones put everything inside an
// empty root and nothing inside a root spelled with a trailing slash when
// the path was that root. There is one now.
func TestPathWithin(t *testing.T) {
	for _, tc := range []struct {
		path string
		root string
		want bool
	}{
		{"/a", "/a", true},
		{"/a/b", "/a", true},
		{"/a/b/c", "/a", true},
		// Trailing slashes change nothing, on either side.
		{"/a", "/a/", true},
		{"/a/", "/a", true},
		{"/a/b/", "/a/", true},
		{"/a//b", "/a", true},
		// A sibling sharing a prefix is not inside.
		{"/ab", "/a", false},
		{"/a-b/c", "/a", false},
		// ".." is resolved as written.
		{"/a/b/../../c", "/a", false},
		{"/a/../a/b", "/a", true},
		{"/a/..", "/a", false},
		{"/a/b", "/a/c/..", true},
		// A name that merely starts with dots is an ordinary child.
		{"/a/..b", "/a", true},
		{"/a/...", "/a", true},
		// Case is part of the name: comparing locations on a
		// case-insensitive volume is the caller's job (resolve first).
		{"/A/b", "/a", false},
		{"/a/B", "/a/b", false},
		// The file system root contains every absolute path.
		{"/", "/", true},
		{"/x/y", "/", true},
		// Nothing is inside an empty root, and an empty path is nowhere.
		{"/a", "", false},
		{"", "/a", false},
		{"", "", false},
		// Relative and absolute never mix.
		{"a/b", "/a", false},
		{"/a/b", "a", false},
		{"a/b", "a", true},
		{"b", "a", false},
		// Above the root.
		{"/", "/a", false},
		{"/a", "/a/b", false},
	} {
		if got := PathWithin(tc.path, tc.root); got != tc.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

// PathWithin is lexical: a path through a symlink is inside the link's
// target only once the caller resolves it, which every caller comparing
// file system locations does.
func TestPathWithinSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(target, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	through := filepath.Join(link, "inside")
	if PathWithin(through, target) {
		t.Fatal("a path through a symlink counted as inside its target without resolving it")
	}
	resolvedPath, err := filepath.EvalSymlinks(through)
	if err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if !PathWithin(resolvedPath, resolvedRoot) {
		t.Fatalf("resolved %q is not within resolved %q", resolvedPath, resolvedRoot)
	}
	// A link inside the root that points out of it is outside once resolved.
	outside := t.TempDir()
	escape := filepath.Join(target, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	resolvedEscape, err := filepath.EvalSymlinks(escape)
	if err != nil {
		t.Fatal(err)
	}
	if PathWithin(resolvedEscape, resolvedRoot) {
		t.Fatalf("a link out of the root counted as inside it once resolved: %q", resolvedEscape)
	}
}
