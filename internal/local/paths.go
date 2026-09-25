package local

import (
	"os"
	"path/filepath"
	"strings"
)

// CanonicalPath is one spelling for every spelling of a location: path made
// absolute, with its existing symlinks resolved (ResolveExistingSymlinks),
// and each existing component spelled the way its directory lists it, so a
// different case on a case-insensitive volume (APFS and HFS+ are by default)
// comes out the same too. The part of path that does not exist is kept as
// written, cleaned. It reads the directories along the path, so it is for
// identities computed once (a launchd label, an installation), not for
// every file of a scan.
func CanonicalPath(path string) string {
	resolved, err := ResolveExistingSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	volume := filepath.VolumeName(resolved)
	parts := strings.Split(strings.TrimPrefix(resolved[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for i, part := range parts {
		if part == "" {
			continue
		}
		spelled, exists := listedSpelling(current, part)
		if !exists {
			return filepath.Join(append([]string{current}, parts[i:]...)...)
		}
		current = filepath.Join(current, spelled)
	}
	return current
}

// listedSpelling is name as dir lists it: name itself when dir holds an entry
// spelled exactly so, otherwise the entry that differs from it only in case
// and is the same file. exists is false when name is not in dir at all.
func listedSpelling(dir, name string) (spelled string, exists bool) {
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return name, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return name, true
	}
	for _, e := range entries {
		if e.Name() == name {
			return name, true
		}
	}
	for _, e := range entries {
		if !strings.EqualFold(e.Name(), name) {
			continue
		}
		if other, err := os.Lstat(filepath.Join(dir, e.Name())); err == nil && os.SameFile(info, other) {
			return e.Name(), true
		}
	}
	return name, true
}

// SameLocation reports whether a and b name one location: the same
// CanonicalPath, or two existing paths that are the same file (os.SameFile),
// which also covers a spelling CanonicalPath cannot line up, such as another
// Unicode normalization of a name.
func SameLocation(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	if CanonicalPath(a) == CanonicalPath(b) {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

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
