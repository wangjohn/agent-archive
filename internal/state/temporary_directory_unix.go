//go:build darwin || linux

package state

import (
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func temporaryQuotaLock(root *os.Root, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := root.OpenFile("temporary-quota", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			// Concurrent first creation through independent roots can race
			// os.Root's no-follow path walk. Retry that transient absence only
			// within the original deadline; all other open errors still fail.
			if !errors.Is(err, os.ErrNotExist) || !time.Now().Before(deadline) {
				return nil, err
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			_ = f.Close()
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				return nil, err
			}
			if time.Now().After(deadline) {
				return nil, local.ErrBusy
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		release := func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
		held, e := f.Stat()
		named, n := root.Stat("temporary-quota")
		if e == nil && n == nil && os.SameFile(held, named) {
			return release, nil
		}
		release()
		if e != nil || n != nil || time.Now().After(deadline) {
			return nil, errors.Join(ErrAdmissionStageRecovery, e, n)
		}
	}
}
