package discovery

import "path/filepath"

func canonicalProjectPath(path string) string {
	path = filepath.Clean(path)
	for ancestor := path; ; ancestor = filepath.Dir(ancestor) {
		if root, err := filepath.EvalSymlinks(ancestor); err == nil {
			rel, _ := filepath.Rel(ancestor, path)
			return filepath.Join(root, rel)
		}
		if filepath.Dir(ancestor) == ancestor {
			return path
		}
	}
}
