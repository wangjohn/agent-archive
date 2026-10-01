package nativesessions

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// DirectoryReader supplies native-store enumeration.
type DirectoryReader interface {
	ReadDir(string) ([]fs.DirEntry, error)
}

// StoreRoot names a Claude projects store or Codex sessions/archived store.
type StoreRoot struct {
	Harness   string
	Path      string
	Recursive bool
}

// Ref is a process-local native transcript reference.
type Ref struct {
	Harness string
	Path    string
	Store   string
}

// WalkCoverage distinguishes failed enumeration from an empty store.
type WalkCoverage struct {
	Enumerated        int
	UnreadableFolders int
	RootUnreadable    bool
	Complete          bool
}

// Walk enumerates known layouts, bounded before callbacks and traversal continue.
// A zero file cap is unlimited for import's complete planning mode.
func Walk(ctx context.Context, files DirectoryReader, root StoreRoot, maxFiles int, visit func(Ref) (bool, error)) (WalkCoverage, error) {
	c := WalkCoverage{Complete: true}
	if files == nil {
		return c, errors.New("native filesystem is required")
	}
	var walk func(string, int) (bool, error)
	walk = func(dir string, depth int) (bool, error) {
		if err := ctx.Err(); err != nil {
			c.Complete = false
			return false, err
		}
		entries, err := files.ReadDir(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
				c.Complete = false
				if depth == 0 {
					c.RootUnreadable = true
				} else {
					c.UnreadableFolders++
				}
			}
			return true, nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				c.Complete = false
				return false, err
			}
			// Stop before inspecting another entry or descending into another
			// directory. Non-transcript entries still cost traversal work.
			if maxFiles > 0 && c.Enumerated >= maxFiles {
				c.Complete = false
				return false, nil
			}
			path := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if root.Harness == "claude" && depth == 0 || root.Harness == "codex" && root.Recursive {
					more, err := walk(path, depth+1)
					if err != nil || !more {
						return more, err
					}
				}
				continue
			}
			valid := e.Type().IsRegular() && (root.Harness == "claude" && depth == 1 && strings.HasSuffix(e.Name(), ".jsonl") || root.Harness == "codex" && strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), ".jsonl"))
			if !valid {
				continue
			}
			c.Enumerated++
			more, err := visit(Ref{root.Harness, path, root.Path})
			if err != nil || !more {
				c.Complete = false
				return false, err
			}
		}
		return true, nil
	}
	_, err := walk(root.Path, 0)
	return c, err
}
