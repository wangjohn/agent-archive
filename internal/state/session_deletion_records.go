package state

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// Hold each owning directory and regular-file identity before local forget.
// A corrupt later directory must refuse before an earlier record is unlinked.
type deletionRecords struct {
	root  *os.Root
	home  string
	dirs  map[string]*os.Root
	files map[string]os.FileInfo
}

func (s *Store) pinDeletionRecords(paths []string) (_ *deletionRecords, err error) {
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return nil, err
	}
	r := &deletionRecords{root: root, home: s.home, dirs: map[string]*os.Root{".": root}, files: map[string]os.FileInfo{}}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.close())
		}
	}()
	for _, path := range paths {
		rel, e := filepath.Rel(s.home, path)
		if e != nil || rel == "." || !local.PathWithin(path, s.home) {
			return nil, ErrAdmissionStageRecovery
		}
		directory := filepath.Dir(rel)
		dir, e := r.directory(directory)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		info, e := dir.Lstat(filepath.Base(rel))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, ErrAdmissionStageRecovery
		}
		r.files[rel] = info
	}
	return r, nil
}

func (r *deletionRecords) directory(path string) (*os.Root, error) {
	if dir := r.dirs[path]; dir != nil {
		return dir, nil
	}
	parent, err := r.directory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	dir, err := openRemovalDirectory(parent, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	r.dirs[path] = dir
	return dir, nil
}

func (r *deletionRecords) currentHome() error {
	held, err := r.root.Stat(".")
	if err != nil {
		return err
	}
	current, err := os.Stat(r.home)
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return ErrAdmissionStageRecovery
	}
	return nil
}

func (r *deletionRecords) remove(path string) error {
	rel, err := filepath.Rel(r.home, path)
	if err != nil {
		return err
	}
	dir, err := r.directory(filepath.Dir(rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := filepath.Base(rel)
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	before := r.files[rel]
	if before == nil || !info.Mode().IsRegular() || !os.SameFile(before, info) || before.Size() != info.Size() || !before.ModTime().Equal(info.ModTime()) {
		return ErrAdmissionStageRecovery
	}
	return dir.Remove(name)
}

func (r *deletionRecords) removeEmptySession(id string) error {
	dir, err := r.directory(filepath.Join("sessions", id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := removalDirectoryEntries(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return nil
	}
	parent := r.dirs["sessions"]
	info, err := parent.Lstat(id)
	if err != nil {
		return err
	}
	held, err := dir.Stat(".")
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(info, held) {
		return ErrAdmissionStageRecovery
	}
	return parent.Remove(id)
}

func (r *deletionRecords) close() (err error) {
	for _, dir := range r.dirs {
		err = errors.Join(err, dir.Close())
	}
	return err
}
