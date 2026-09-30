//go:build unix

package local

import (
	"errors"
	"os"
	"syscall"
)

// NamedLock is a flock, which only Unix systems have: the program is
// Unix-only, and a build for anything else fails here.

// lockOpened flocks f, which was opened at path, and reports whether path
// still names f's file once the lock is held. When it does not, or on error,
// f is unlocked and closed; otherwise release does both.
func lockOpened(path string, f *os.File) (release func(), current bool, e error) {
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = f.Close()
		if errors.Is(e, syscall.EWOULDBLOCK) || errors.Is(e, syscall.EAGAIN) {
			return nil, false, ErrBusy
		}
		return nil, false, e
	}
	release = func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	locked, e := f.Stat()
	if e != nil {
		release()
		return nil, false, e
	}
	named, e := os.Stat(path)
	if e == nil && os.SameFile(locked, named) {
		return release, true, nil
	}
	release()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, false, e
	}
	return nil, false, nil
}
