package sourcefacts

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"path/filepath"
	"strings"
)

// ConfiguredOwner applies the nearest explicit rule, including exclusions.
func ConfiguredOwner(projects []archive.ProjectActivation, cwd string, resolve func(string) string) (archive.ProjectActivation, bool) {
	best := -1
	var owner archive.ProjectActivation
	for _, p := range projects {
		root := resolve(p.Root)
		if local.PathWithin(cwd, root) && len(root) > best {
			owner = p
			best = len(root)
		}
	}
	return owner, best >= 0
}

// WorktreeMain resolves bounded Git metadata without running Git. It requires
// the common directory to be an existing .git and validates the reverse link.
func WorktreeMain(checkout string, read func(string) ([]byte, error), exists func(string) bool, strict bool) (string, bool) {
	limited := func(path string) (string, error) {
		b, err := read(path)
		if err != nil || len(b) > 4096 {
			return "", errors.New("unavailable Git metadata")
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := limited(filepath.Join(checkout, ".git"))
	if err != nil {
		if !strict {
			return checkout, true
		}
		return "", false
	}
	gd, ok := strings.CutPrefix(strings.SplitN(line, "\n", 2)[0], "gitdir:")
	if !ok {
		if !strict {
			return checkout, true
		}
		return "", false
	}
	gd = strings.TrimSpace(gd)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(checkout, gd)
	}
	gd = filepath.Clean(gd)
	if !exists(gd) {
		return "", false
	}
	common, err := limited(filepath.Join(gd, "commondir"))
	if err != nil {
		if !strict {
			return checkout, true
		}
		return "", false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gd, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		if !strict {
			return checkout, true
		}
		return "", false
	}
	if !exists(common) {
		return "", false
	}
	if strict {
		// The .git file must point into this repository's worktree registry, and
		// its gitdir backlink must name this exact checkout, preventing cycles.
		if !local.PathWithin(gd, filepath.Join(common, "worktrees")) {
			return "", false
		}
		backlink, e := limited(filepath.Join(gd, "gitdir"))
		if e != nil || filepath.Clean(backlink) != filepath.Join(checkout, ".git") {
			return "", false
		}
	}
	return filepath.Dir(common), true
}
