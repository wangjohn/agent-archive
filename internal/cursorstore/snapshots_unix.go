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
// the returned file holds until it is closed. The file is locked under
// another name and only then renamed into place, so a sweep never finds it
// unlocked: had the lock been taken after the file appeared, a sweep's
// snapshotInUse could hold it at that moment and fail the read. placed, if
// not nil, runs once the file is in place, before lockSnapshot returns.
func lockSnapshot(dir string, placed func(lockPath string)) (*os.File, error) {
	newPath := filepath.Join(dir, snapshotLockNewName)
	f, err := os.OpenFile(newPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	// dir is the Reader's own new 0700 directory, so nothing else is
	// there for the rename to replace.
	lockPath := filepath.Join(dir, snapshotLockName)
	if err := os.Rename(newPath, lockPath); err != nil {
		_ = f.Close()
		return nil, err
	}
	if placed != nil {
		// Should placed not return (a test's t.Fatal), nothing else would
		// ever close f and release its lock.
		returned := false
		defer func() {
			if !returned {
				_ = f.Close()
			}
		}()
		placed(lockPath)
		returned = true
	}
	return f, nil
}

// snapshotInUse reports whether a Reader holds dir's lock. A directory
// without a lock file is not reported in use: its Reader may still be
// starting, or may have died before placing the file.
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
