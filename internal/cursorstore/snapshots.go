package cursorstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
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

// snapshotRootPath is where snapshots go (platform.Locations.SnapshotRoot: on
// macOS a directory of this user's own in the per-user temporary directory,
// which the user ID in its name keeps apart from other users' where it is
// shared; on Linux a folder under the XDG cache home), or "" on a system the
// program does not know, or where no cache home is known, where no snapshot
// is taken. A test's SnapshotTempDirForTesting replaces the temporary
// directory.
func snapshotRootPath() string {
	if SnapshotTempDirForTesting != "" {
		return filepath.Join(SnapshotTempDirForTesting, platform.SnapshotDirName(os.Getuid()))
	}
	return currentSnapshotLocations().SnapshotRoot()
}

// snapshotCacheDir is agent-archive's folder in the cache home, the parent of
// the root on Linux, and "" where there is none (macOS, a test).
func snapshotCacheDir() string {
	if SnapshotTempDirForTesting != "" {
		return ""
	}
	return currentSnapshotLocations().SnapshotCacheDir()
}

// currentSnapshotLocations are the locations of the running system for this
// process's environment and account.
func currentSnapshotLocations() platform.Locations {
	return snapshotLocations(platform.Current(), os.Getenv, accountHome)
}

// accountHome is the account's home directory from the user database (not
// $HOME), "" when it cannot be read.
func accountHome() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.HomeDir
}

// snapshotLocations are the locations of system, reading the environment
// through getenv, the account's home directory as accountHome finds it, and
// the system's per-user temporary directory as this package finds it. There
// is no $HOME: nothing about the snapshot root depends on it. Only Linux
// places the root by the account's home, so only Linux calls accountHome (a
// user database read): on macOS finding the root does what it always did.
func snapshotLocations(system platform.OS, getenv func(string) string, accountHome func() string) platform.Locations {
	home := ""
	if system == platform.Linux && accountHome != nil {
		home = accountHome()
	}
	return platform.NewLocations(system, "", getenv, platform.LocationDeps{
		DarwinUserTempDir: darwinUserTempDir,
		ProcessTempDir:    os.TempDir,
		UID:               os.Getuid(),
		AccountHome:       home,
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
// program knows, or on Linux that neither XDG_CACHE_HOME nor the account's
// home directory is known, so there is no place to keep a copy of Cursor's
// chats.
var errSnapshotUnsupportedSystem = errors.New("the Cursor snapshot directory is not known here (an operating system this program does not know, or no cache or home directory)")

// SnapshotRoot returns the directory snapshots are taken in, creating it
// 0700 if needed. It must be a real directory (not a link), owned by this
// user, with mode 0700; otherwise no snapshot is taken, and the error says
// which directory to remove. On Linux its parent, agent-archive's folder in
// the cache home, must be yours and writable by you alone too, and gets a
// CACHEDIR.TAG.
func SnapshotRoot() (string, error) {
	return preparedSnapshotRoot(snapshotRootPath(), snapshotCacheDir())
}

// preparedSnapshotRoot is SnapshotRoot for the paths snapshotRootPath and
// snapshotCacheDir answered: "" is a system with no snapshot directory, which
// fails closed rather than falling back to a directory nobody chose.
func preparedSnapshotRoot(root, cacheDir string) (string, error) {
	if root == "" {
		return "", errSnapshotUnsupportedSystem
	}
	if cacheDir != "" {
		if err := prepareCacheDir(cacheDir); err != nil {
			return "", err
		}
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

// cacheDirTag is the contents of the CACHEDIR.TAG file: the signature the
// Cache Directory Tagging Specification (https://bford.info/cachedir/)
// requires at the start, followed by comment lines.
const cacheDirTag = "Signature: 8a477f597d28d172789f06886806bc55\n" +
	"# This file is a cache directory tag created by agent-archive.\n" +
	"# For information about cache directory tags, see:\n" +
	"#\thttps://bford.info/cachedir/\n"

// cacheDirTagName is the tag file's name, fixed by the specification.
const cacheDirTagName = "CACHEDIR.TAG"

// errCacheDirNotPrivate means agent-archive's folder in the cache home exists
// but is not a directory of this user's that nobody else can write to, or the
// cache home is one every account can write to (without the sticky bit). A
// directory another account can write to could have the directory inside it
// renamed away and replaced between the checks and the copy.
var errCacheDirNotPrivate = errors.New("agent-archive's folder in the cache directory is not private to this user")

// prepareCacheDir makes dir, agent-archive's folder in the cache home (and
// the cache home itself, 0700 as the XDG Base Directory specification asks,
// if it is missing), and checks it is a real directory of this user's that no
// one else can write to. It then leaves a CACHEDIR.TAG in it when it has
// none, so a backup tool that honors the convention skips the copies of
// Cursor's chats under it. The tag is best effort: a copy is removed as soon
// as it is read, so a tool that backs up a leftover one is an inconvenience,
// and a failure to write the tag must not stop the read.
//
// A cache home that was already there is the user's and is left as it is
// (its mode is never changed), unless every account can write to it without
// the sticky bit: anyone could then rename dir away and put their own in its
// place between the checks and the copy, so it is refused, the way dir is.
func prepareCacheDir(dir string) error {
	cache := filepath.Dir(dir)
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return errors.New("create the cache directory")
	}
	if info, err := os.Stat(cache); err != nil || !info.IsDir() {
		return errors.New("inspect the cache directory")
	} else if info.Mode().Perm()&0o002 != 0 && info.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("%w: every account can write to the cache directory %s; set XDG_CACHE_HOME to a directory of yours, or take that write access away (chmod o-w)", errCacheDirNotPrivate, cache)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return errors.New("create agent-archive's folder in the cache directory")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return errors.New("inspect agent-archive's folder in the cache directory")
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: %s must be a directory of yours that only you can write to, not a link; remove it so it is created again", errCacheDirNotPrivate, dir)
	}
	writeCacheDirTag(dir)
	return nil
}

// writeCacheDirTag creates dir's CACHEDIR.TAG unless something is there
// already, a tag of the user's own included.
func writeCacheDirTag(dir string) {
	f, err := os.OpenFile(filepath.Join(dir, cacheDirTagName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, writeErr := f.WriteString(cacheDirTag)
	if closeErr := f.Close(); writeErr != nil || closeErr != nil {
		_ = os.Remove(f.Name())
	}
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
