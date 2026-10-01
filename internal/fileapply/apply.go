// Package fileapply applies generic byte plans with shared rollback and symlink semantics.
package fileapply

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"os"
	"path/filepath"
)

// Change is the value-only byte plan applied by this package.
type Change = filechange.Change

// Applied reports whether the file at c.Path is as c leaves it: deleted, or
// holding After.
func Applied(c Change) bool {
	current, err := os.ReadFile(c.Path)
	if c.Delete {
		return os.IsNotExist(err)
	}
	return err == nil && string(current) == string(c.After)
}

// Unapplied reports whether the file at c.Path is as c found it: holding
// Before, or absent when it did not exist.
func Unapplied(c Change) bool {
	current, err := os.ReadFile(c.Path)
	if !c.Existed {
		return os.IsNotExist(err)
	}
	return err == nil && string(current) == string(c.Before)
}

// ErrChanged reports that a file changed after its Change was planned.
// Apply wraps it with the file's path; the caller says how to retry.
var ErrChanged = errors.New("changed while it was being updated")

// Apply rolls back already written files on failure. It refuses a configuration
// changed since the plan was prepared, rather than overwriting concurrent edits.
func Apply(changes []Change) error { return ApplyWithTarget(changes, ResolveTarget) }

// ApplyWithTarget applies changes using an injected target resolver.
// or a stand-in a test gives to point a write somewhere else.
func ApplyWithTarget(changes []Change, writeTarget func(path string) (string, error)) error {
	applied := []Change{}
	for _, c := range changes {
		current, err := os.ReadFile(c.Path)
		exists := err == nil
		if (err != nil && !os.IsNotExist(err)) || exists != c.Existed || string(current) != string(c.Before) {
			return errors.Join(fmt.Errorf("%s %w", c.Path, ErrChanged), rollback(applied))
		}
		target, err := writeTarget(c.Path)
		if err != nil {
			return errors.Join(fmt.Errorf("cannot update %s: %w", c.Path, err), rollback(applied))
		}
		if c.Delete && target != c.Path {
			// It became a link since it was planned: the file it points to
			// is kept, emptied, as for any link (see PlanRemovalOf).
			c.Delete = false
		}
		if c.Delete {
			if err = os.Remove(c.Path); err != nil {
				return errors.Join(fmt.Errorf("cannot remove %s: %w", c.Path, err), rollback(applied))
			}
			applied = append(applied, c)
			continue
		}
		undo, err := snapshot(target)
		if err != nil {
			return errors.Join(fmt.Errorf("cannot update %s: %w", c.Path, err), rollback(applied))
		}
		if err = writeFile(target, c.After, c.Mode); err != nil {
			// writeFile replaces the file only by its final rename, so a
			// failure left the file as it was: only directories it created
			// are taken back.
			undo.removeCreatedDirs()
			return errors.Join(fmt.Errorf("cannot update %s: %w", c.Path, err), rollback(applied))
		}
		// What the application will read is the file through c.Path, links
		// and all; a write that landed anywhere else is not a success, and
		// is taken back along with the earlier changes.
		if written, err := os.ReadFile(c.Path); err != nil || string(written) != string(c.After) {
			return errors.Join(fmt.Errorf("cannot update %s: the file written (%s) does not read back through it", c.Path, target), undo.restore(), rollback(applied))
		}
		applied = append(applied, c)
	}
	return nil
}

// Rollback undoes applied changes in reverse order: each file is restored to
// its Before content, or removed if the change created it. A file whose
// content is no longer the change's After was edited since, so it is left
// untouched and reported as needing manual recovery. All failures are joined
// into the returned error.
func Rollback(changes []Change) error { return rollback(changes) }

func rollback(changes []Change) error {
	var failures []error
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if !Applied(c) {
			failures = append(failures, fmt.Errorf("%s changed; manual recovery required", c.Path))
			continue
		}
		var e error
		if c.Existed {
			e = atomicWrite(c.Path, c.Before, c.Mode)
		} else {
			// Remove what was created: through a symlink, its target.
			var target string
			if target, e = ResolveTarget(c.Path); e == nil {
				e = os.Remove(target)
			}
		}
		if e != nil {
			failures = append(failures, e)
		}
	}
	return errors.Join(failures...)
}

// ResolveTarget follows path through any symlinks to the file they name,
// which need not exist yet.
func ResolveTarget(path string) (string, error) {
	for range 40 {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			// A missing file may still sit in a symlinked directory; that is
			// resolved by the rename itself.
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		link, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			// Relative to the directory the link really sits in, which is
			// not filepath.Dir(path) when that directory is itself a link
			// (~/.claude -> dotfiles/claude).
			dir, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			link = filepath.Join(dir, link)
		}
		path = link
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}

// atomicWrite replaces the file at path with data by renaming a temporary
// file into place. When path is a symlink (a dotfile manager's link into a
// repository, say) it writes the file the link names, in that file's own
// directory, so the link survives and the repository copy is the one
// updated.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	target, err := ResolveTarget(path)
	if err != nil {
		return err
	}
	return writeFile(target, data, mode)
}

// priorFile is what was at a path before Apply wrote it, so a write that
// must be taken back can be: the old content, or its absence together with
// the directories the write created.
type priorFile struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
	created []string // directories that did not exist, deepest first
}

func snapshot(path string) (priorFile, error) {
	prior := priorFile{path: path}
	info, err := os.Stat(path)
	switch {
	case err == nil:
		// A file that is there but cannot be read could not be put back,
		// so it is refused rather than overwritten.
		data, err := os.ReadFile(path)
		if err != nil {
			return priorFile{}, err
		}
		prior.mode, prior.data, prior.existed = info.Mode().Perm(), data, true
	case !os.IsNotExist(err):
		return priorFile{}, err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(dir); err == nil || filepath.Dir(dir) == dir {
			break
		}
		prior.created = append(prior.created, dir)
	}
	return prior, nil
}

// restore puts the path back as snapshot found it. Directories it created
// are removed only while empty.
func (p priorFile) restore() error {
	if p.existed {
		return writeFile(p.path, p.data, p.mode)
	}
	if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	p.removeCreatedDirs()
	return nil
}

// removeCreatedDirs removes the directories a write created, while empty.
func (p priorFile) removeCreatedDirs() {
	for _, dir := range p.created {
		_ = os.Remove(dir) // fails, and keeps the directory, if it is not empty
	}
}

// writeFile atomically replaces the regular file at path (no link
// following: see atomicWrite) with data.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-")
	if err != nil {
		return err
	}
	name := f.Name()
	// After a successful rename there is nothing left at name to remove.
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
