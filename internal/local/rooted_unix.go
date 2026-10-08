//go:build unix

package local

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// RootedLockWait locks only the file in a held home and verifies its identity.
func RootedLockWait(h *RootedHome, name string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		if err := h.Check(); err != nil {
			return nil, err
		}
		f, err := h.Root.OpenFile(name, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			// A concurrent first creator can race a rooted path lookup.
			// Retry only absence, inside the same held root and deadline.
			if errors.Is(err, os.ErrNotExist) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("open rooted lock: %w", err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			_ = f.Close()
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				return nil, fmt.Errorf("flock rooted lock: %w", err)
			}
			if time.Now().After(deadline) {
				return nil, ErrBusy
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		release := func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
		held, e := f.Stat()
		named, n := h.Root.Lstat(name)
		if e == nil && n == nil && named.Mode().IsRegular() && os.SameFile(held, named) {
			h.lockMu.Lock()
			if h.locks == nil {
				h.locks = map[string]os.FileInfo{}
			}
			h.locks[name] = held
			h.lockMu.Unlock()
			originalRelease := release
			release = func() { h.lockMu.Lock(); delete(h.locks, name); h.lockMu.Unlock(); originalRelease() }
			if e = h.Check(); e == nil {
				return release, nil
			}
		}
		release()
		return nil, errors.Join(errors.New("archive lock changed while opening"), e, n)
	}
}
