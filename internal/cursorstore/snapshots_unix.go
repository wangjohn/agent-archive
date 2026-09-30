//go:build unix

package cursorstore

import (
	"os"
	"path/filepath"
	"syscall"
)

// The snapshot locks are flocks, which only Unix systems have: the program
// is Unix-only, and a build for anything else fails here.

// lockSnapshot creates dir's lock file and takes its exclusive lock, which
// the returned file holds until it is closed.
func lockSnapshot(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, snapshotLockName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// snapshotInUse reports whether a Reader holds dir's lock. A directory
// without a lock file was left by a process that died before taking it.
func snapshotInUse(dir string) bool {
	f, err := os.Open(filepath.Join(dir, snapshotLockName))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // closing f releases it anyway
	return false
}
