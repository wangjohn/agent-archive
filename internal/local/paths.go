package local

import (
	"path/filepath"
	"strings"
)

// PathWithin reports whether path is root or lies beneath it. It compares
// the two lexically, after filepath.Clean: a trailing slash changes nothing,
// ".." is resolved against the path as written (so /a/b/../../c is not in
// /a), and names are compared exactly as spelled, case included. It does
// not touch the file system: a caller comparing file system locations
// resolves symlinks first (filepath.EvalSymlinks) and passes both resolved
// paths. Nothing lies within an empty root, and a relative path is never
// within an absolute root or the reverse.
func PathWithin(path, root string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
