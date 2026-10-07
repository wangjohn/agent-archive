package local

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// RootedHome holds an existing canonical archive home through every write.
// The named home must continue to identify the held directory.
type RootedHome struct {
	Root   *os.Root
	path   string
	info   os.FileInfo
	lockMu sync.Mutex
	locks  map[string]os.FileInfo
}

// OpenRootedHome opens an existing home without creating anything.
func OpenRootedHome(home string) (*RootedHome, error) {
	absolute, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, errors.Join(errors.New("archive home must be an existing canonical directory"), err)
	}
	before, err := os.Lstat(absolute)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(errors.New("unsafe archive home"), err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	h := &RootedHome{Root: root, path: absolute, info: before}
	if err != nil || !os.SameFile(before, opened) {
		_ = root.Close()
		return nil, errors.Join(errors.New("archive home changed while opening"), err)
	}
	if err = h.Check(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return h, nil
}

// Check refuses a renamed or replaced archive home; writes never reopen it.
func (h *RootedHome) Check() error {
	named, err := os.Lstat(h.path)
	if err != nil || !os.SameFile(h.info, named) || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 || named.Mode().Perm()&0077 != 0 {
		return errors.Join(errors.New("archive home changed; retry after restoring its location"), err)
	}
	h.lockMu.Lock()
	defer h.lockMu.Unlock()
	for name, held := range h.locks {
		named, e := h.Root.Lstat(name)
		if e != nil || !named.Mode().IsRegular() || !os.SameFile(held, named) {
			return errors.Join(errors.New("archive lock changed while held"), e)
		}
	}
	return nil
}

// Close releases the held directory.
func (h *RootedHome) Close() error { return h.Root.Close() }

// RootedWrite atomically encodes one value through a held private directory.
// The caller bounds allocation and serializes writers before calling it.
func RootedWrite(root *os.Root, name string, value any) error {
	return RootedAtomicWrite(root, name, func(w io.Writer) error { return json.NewEncoder(w).Encode(value) })
}

// RootedAtomicWrite writes, syncs and renames inside the held directory. Failed
// temporary removal is returned; interrupted files remain visible to accounting.
func RootedAtomicWrite(root *os.Root, name string, write func(io.Writer) error) (err error) {
	token, err := ID()
	if err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".pending-"+token)
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		e := root.Remove(temp)
		if !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = root.Rename(temp, name); err != nil {
		return err
	}
	d, err := root.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
