package cursorstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// snapshotPrefix names the private directories that hold database copies.
const snapshotPrefix = "cursor-snapshot-"

// staleSnapshotAge is how old a leftover snapshot directory must be before
// it is removed. Only a killed process leaves one, and no read lasts this
// long.
const staleSnapshotAge = time.Hour

// snapshotRootPath is where snapshots go: a directory of this user's own in
// the system temporary directory. A copy holds every Cursor chat, including
// those of projects that are not archived, so it stays out of the archive
// home, which may be backed up or synced. On macOS os.TempDir is $TMPDIR, a
// per-user directory under /var/folders that Time Machine excludes; the user
// ID in the name keeps users apart where the temporary directory is shared.
func snapshotRootPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-archive-cursor-%d", os.Getuid()))
}

// errSnapshotRootNotPrivate means the snapshot directory exists but is not a
// directory of this user's that only this user can open.
var errSnapshotRootNotPrivate = errors.New("the Cursor snapshot directory is not private to this user")

// SnapshotRoot returns the directory snapshots are taken in, creating it
// 0700 if needed. It must be a real directory (not a link), owned by this
// user, with mode 0700; otherwise no snapshot is taken.
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
		return "", errSnapshotRootNotPrivate
	}
	return root, nil
}

// RemoveStaleSnapshots removes snapshot directories a killed process left
// behind, so a copy of Cursor's chats does not outlive its read. Recent ones
// may belong to a read in progress and are left alone.
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
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > staleSnapshotAge {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}
