//go:build unix

package discovery

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/local"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const privateIndexPrefix = "snapshot-"

const privateIndexLock = "in-use.lock"

func indexSnapshotRoot(temp string) string {
	return filepath.Join(temp, "agent-archive-codex-index-"+strconv.Itoa(os.Getuid()))
}

func privateOwnedDirectory(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Getuid())
}

func prepareIndexSnapshotRoot() (string, error) {
	root := indexSnapshotRoot(os.TempDir())
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if !privateOwnedDirectory(root) {
		return "", errors.New("private index root is not owned with private permissions")
	}
	return root, nil
}

func lockPrivateIndex(dir string) (*os.File, error) {
	path := filepath.Join(dir, privateIndexLock+".new")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := os.Rename(path, filepath.Join(dir, privateIndexLock)); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// sweepPrivateIndexes uses a bounded durable getdents cursor inside only this
// agent's private root. Live locks protect copies even after clock rollback.
// Unknown names, unsafe modes/ownership and symlinks are never removed.
func sweepPrivateIndexes(ctx context.Context, root string, now time.Time) error {
	if !privateOwnedDirectory(root) {
		return nil
	}
	owner, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
	cursorPath := filepath.Join(root, "cleanup.cursor")
	var offset int64
	if info, err := owner.Lstat("cleanup.cursor"); err == nil && info.Mode().IsRegular() && info.Size() <= 64 {
		if file, err := owner.OpenFile("cleanup.cursor", os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(file, 64))
			_ = file.Close()
			if readErr == nil {
				offset, _ = strconv.ParseInt(string(raw), 10, 64)
			}
		}
	}
	if offset < 0 {
		offset = 0
	}
	names, next, complete, err := readBatch(directory{Root: root, Path: ".", Offset: offset})
	if err != nil {
		return err
	}
	var cleanup []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(name, privateIndexPrefix) {
			continue
		}
		dir := filepath.Join(root, name)
		if !privateOwnedDirectory(dir) {
			continue
		}
		info, err := os.Lstat(dir)
		if err != nil {
			continue
		}
		age := time.Hour
		lockPath := filepath.Join(name, privateIndexLock)
		lockInfo, infoErr := owner.Lstat(lockPath)
		if infoErr == nil && !lockInfo.Mode().IsRegular() {
			continue
		}
		lock, err := owner.OpenFile(lockPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			stat, statErr := lock.Stat()
			if statErr != nil || !stat.Mode().IsRegular() {
				_ = lock.Close()
				continue
			}
			if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
				_ = lock.Close()
				continue
			}
			info = stat
			age = time.Minute
		} else if pending, pendingErr := owner.Lstat(filepath.Join(name, privateIndexLock+".new")); pendingErr == nil {
			info = pending
		}
		if now.Sub(info.ModTime()) > age {
			cleanup = append(cleanup, os.RemoveAll(dir))
		}
		if lock != nil {
			cleanup = append(cleanup, lock.Close())
		}
	}
	if complete {
		next = 0
	}
	// All writer calls are serialized by collector ownership in production;
	// the cursor is only scheduling, never deletion authority.
	cleanup = append(cleanup, local.WriteBytes(cursorPath, []byte(strconv.FormatInt(next, 10))))
	return errors.Join(cleanup...)
}
