package cursorstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/platform"
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

// snapshotRootPath is where snapshots go (platform.Locations.SnapshotRoot: a
// directory of this user's own in the per-user temporary directory, which the
// user ID in its name keeps apart from other users' where it is shared), or
// "" on a system the program does not know, where no snapshot is taken. A
// test's SnapshotTempDirForTesting replaces the temporary directory.
func snapshotRootPath() string {
	if SnapshotTempDirForTesting != "" {
		return filepath.Join(SnapshotTempDirForTesting, platform.SnapshotDirName(os.Getuid()))
	}
	return snapshotLocations(platform.Current(), os.Getenv).SnapshotRoot()
}

// snapshotLocations are the locations of system, reading the environment
// through getenv and the system's per-user temporary directory as this
// package finds it. There is no home: nothing about the snapshot root
// depends on one.
func snapshotLocations(system platform.OS, getenv func(string) string) platform.Locations {
	return platform.NewLocations(system, "", getenv, platform.LocationDeps{
		DarwinUserTempDir: darwinUserTempDir,
		ProcessTempDir:    os.TempDir,
		UID:               os.Getuid(),
	})
}

// SnapshotTempDirForTesting replaces, when set, the per-user temporary
// directory snapshots go under. Only tests set it, so their copies stay in
// folders of their own.
var SnapshotTempDirForTesting string

var (
	darwinTempOnce sync.Once
	darwinTempDir  string
)

// darwinUserTempDir is confstr(_CS_DARWIN_USER_TEMP_DIR), asked of getconf
// once per process so builds need no cgo; "" when it can't be read.
func darwinUserTempDir() string {
	darwinTempOnce.Do(func() {
		out, err := exec.CommandContext(context.Background(), "/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			darwinTempDir = filepath.Clean(strings.TrimSpace(string(out)))
		}
	})
	return darwinTempDir
}

// errSnapshotRootNotPrivate means the snapshot directory exists but is not a
// directory of this user's that only this user can open.
var errSnapshotRootNotPrivate = errors.New("the Cursor snapshot directory is not private to this user")

// errSnapshotUnsupportedSystem means this operating system is not one the
// program knows, so it has no place to keep a copy of Cursor's chats.
var errSnapshotUnsupportedSystem = errors.New("the Cursor snapshot directory is not known on this operating system")

// SnapshotRoot returns the directory snapshots are taken in, creating it
// 0700 if needed. It must be a real directory (not a link), owned by this
// user, with mode 0700; otherwise no snapshot is taken, and the error says
// which directory to remove.
func SnapshotRoot() (string, error) { return preparedSnapshotRoot(snapshotRootPath()) }

// preparedSnapshotRoot is SnapshotRoot for the path snapshotRootPath
// answered: "" is a system with no snapshot directory, which fails closed
// rather than falling back to a directory nobody chose.
func preparedSnapshotRoot(root string) (string, error) {
	if root == "" {
		return "", errSnapshotUnsupportedSystem
	}
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

// abandonedSnapshotAge is how old the lock file of an unlocked snapshot
// directory must be before the directory is removed. A Reader takes the lock
// right after creating the file, and the lock is released only by Close,
// which removes the directory, or by the process dying; so an unlocked lock
// file older than this was left by a process that died (Ctrl-C, a crash).
// The grace only covers the instant between creating the file and locking
// it.
const abandonedSnapshotAge = time.Minute

// ownSnapshots are the snapshot directories this process's Readers created
// and have not closed yet.
var ownSnapshots = struct {
	sync.Mutex
	dirs map[string]bool
}{dirs: map[string]bool{}}

func trackSnapshot(dir string) {
	ownSnapshots.Lock()
	defer ownSnapshots.Unlock()
	ownSnapshots.dirs[dir] = true
}

func untrackSnapshot(dir string) {
	ownSnapshots.Lock()
	defer ownSnapshots.Unlock()
	delete(ownSnapshots.dirs, dir)
}

// RemoveOwnSnapshots removes every snapshot directory this process's
// Readers created and have not closed, even one a backup is still writing
// into. It is for a process about to exit on a signal (a second Ctrl-C,
// SIGTERM, SIGHUP), whose Readers will never be closed: without it the copy
// of every Cursor chat would stay in the temporary folder until a later
// sweep. A Reader whose directory it removed fails its reads afterwards.
func RemoveOwnSnapshots() {
	ownSnapshots.Lock()
	defer ownSnapshots.Unlock()
	for dir := range ownSnapshots.dirs {
		_ = os.RemoveAll(dir)
		delete(ownSnapshots.dirs, dir)
	}
}

// RemoveStaleSnapshots removes snapshot directories a killed process left
// behind, so a copy of Cursor's chats does not outlive its read. Every
// backfill command calls it first, and the collector at the start of every
// pass; any other long-running reader should too. A locked
// one belongs to a read in progress and is left alone. An unlocked one whose
// lock file is over abandonedSnapshotAge old was abandoned and is removed; one
// without a lock file may belong to a read that is just starting, so it is
// removed only once it is over staleSnapshotAge old.
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
		if snapshotInUse(dir) {
			continue
		}
		age := staleSnapshotAge
		info, err := e.Info()
		if lock, lockErr := os.Lstat(filepath.Join(dir, snapshotLockName)); lockErr == nil {
			age, info, err = abandonedSnapshotAge, lock, nil
		}
		if err == nil && time.Since(info.ModTime()) > age {
			_ = os.RemoveAll(dir)
		}
	}
}
