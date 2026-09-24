package cursorstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// snapshotPrefix names the private directories that hold database copies.
const snapshotPrefix = "cursor-snapshot-"

// snapshotLockName is the file in each snapshot directory its Reader holds
// an exclusive flock on while the copy is in use, so a sweep, in this
// process or another, never removes a copy a pass is still reading however
// long the pass lasts.
const snapshotLockName = "in-use.lock"

// staleSnapshotAge is how old a leftover snapshot directory must be before
// it is removed. Only a killed process leaves one, and a directory whose lock
// is held is never removed whatever its age.
const staleSnapshotAge = time.Hour

// snapshotRootPath is where snapshots go: a directory of this user's own in
// the per-user temporary directory. A copy holds every Cursor chat,
// including those of projects that are not archived, so it stays out of the
// archive home, which may be backed up or synced. On macOS that directory
// is under /var/folders, which Time Machine excludes; the user ID in the
// name keeps users apart where the temporary directory is shared.
func snapshotRootPath() string {
	base := SnapshotTempDirForTesting
	if base == "" {
		base = userTempDir(os.Getenv, runtime.GOOS, darwinUserTempDir)
	}
	return filepath.Join(base, fmt.Sprintf("agent-archive-cursor-%d", os.Getuid()))
}

// SnapshotTempDirForTesting, when set, replaces the per-user temporary
// directory snapshots go under. Only tests set it, so their copies stay in
// folders of their own.
var SnapshotTempDirForTesting string

// userTempDir is, on macOS, the per-user temporary directory the system
// reports (DARWIN_USER_TEMP_DIR), whatever $TMPDIR says: it is the folder
// Time Machine excludes, and every process of the user's agrees on it,
// whereas the LaunchAgent that runs the collector sets only
// AGENT_ARCHIVE_HOME (so launchd may leave TMPDIR unset) and a custom TMPDIR
// in a shell would put a copy of every chat elsewhere and split the sweep's
// root. Only if the system can't say is $TMPDIR used, then os.TempDir; the
// same order, from $TMPDIR, elsewhere.
func userTempDir(getenv func(string) string, goos string, darwinTemp func() string) string {
	if goos == "darwin" {
		if dir := darwinTemp(); filepath.IsAbs(dir) {
			return dir
		}
	}
	if dir := getenv("TMPDIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

var (
	darwinTempOnce sync.Once
	darwinTempDir  string
)

// darwinUserTempDir is confstr(_CS_DARWIN_USER_TEMP_DIR), asked of getconf
// once per process so builds need no cgo; "" when it can't be read.
func darwinUserTempDir() string {
	darwinTempOnce.Do(func() {
		out, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			darwinTempDir = filepath.Clean(strings.TrimSpace(string(out)))
		}
	})
	return darwinTempDir
}

// errSnapshotRootNotPrivate means the snapshot directory exists but is not a
// directory of this user's that only this user can open.
var errSnapshotRootNotPrivate = errors.New("the Cursor snapshot directory is not private to this user")

// SnapshotRoot returns the directory snapshots are taken in, creating it
// 0700 if needed. It must be a real directory (not a link), owned by this
// user, with mode 0700; otherwise no snapshot is taken, and the error says
// which directory to remove.
func SnapshotRoot() (string, error) {
	root := snapshotRootPath()
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", errors.New("create the Cursor snapshot directory")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", errors.New("inspect the Cursor snapshot directory")
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
		return "", fmt.Errorf("%w: %s must be a directory of yours with mode 0700, not a link; remove it so it is created again", errSnapshotRootNotPrivate, root)
	}
	return root, nil
}

// lockSnapshot creates dir's lock file and takes its exclusive lock, which
// the returned file holds until it is closed.
func lockSnapshot(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, snapshotLockName), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
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
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// RemoveStaleSnapshots removes snapshot directories a killed process left
// behind, so a copy of Cursor's chats does not outlive its read. Recent ones
// may belong to a read that is just starting, and locked ones to a read in
// progress; both are left alone.
func RemoveStaleSnapshots() {
	root := snapshotRootPath()
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), snapshotPrefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleSnapshotAge && !snapshotInUse(dir) {
			os.RemoveAll(dir)
		}
	}
}
