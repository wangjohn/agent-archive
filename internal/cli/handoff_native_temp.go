package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const nativeTempNamespace = "agent-archive-local-handoffs"

// nativeTempPath isolates launch files by effective user, including shared /tmp.
func nativeTempPath(temp string, uid int) string {
	return filepath.Join(temp, fmt.Sprintf("%s-%d", nativeTempNamespace, uid))
}

// nativeTempRoot checks the dedicated private namespace without following links.
func nativeTempRoot(temp string) (string, error) {
	return nativeTempRootForUID(temp, os.Geteuid())
}

func nativeTempRootForUID(temp string, uid int) (string, error) {
	root := nativeTempPath(temp, uid)
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !privateNativeTempDirectory(info, uid) {
		return "", errors.New("local handoff temporary namespace must be a private directory owned by the current user; remove the conflicting entry or choose another temporary directory")
	}
	return root, nil
}

// pruneNativeHandoffs never scans unrelated temporary entries or follows links.
func pruneNativeHandoffs(temp string, now time.Time) {
	pruneNativeHandoffsForUID(temp, now, os.Geteuid())
}

func pruneNativeHandoffsForUID(temp string, now time.Time, uid int) {
	root := nativeTempPath(temp, uid)
	info, err := os.Lstat(root)
	if err != nil || !privateNativeTempDirectory(info, uid) {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "launch-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !privateNativeTempDirectory(info, uid) || !info.ModTime().Before(now.Add(-handoffMaxAge)) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

func writeNativeLaunchHandoff(temp string, content []byte, now time.Time) (string, error) {
	pruneNativeHandoffs(temp, now)
	root, err := nativeTempRoot(temp)
	if err != nil {
		return "", fmt.Errorf("private local handoff: %w", err)
	}
	dir, err := os.MkdirTemp(root, "launch-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, launchHandoffName)
	if err := writeNewFile(path, content); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}
