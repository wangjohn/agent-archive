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

// nativeTempRoot checks the dedicated private namespace without following links.
func nativeTempRoot(temp string) (string, error) {
	root := filepath.Join(temp, nativeTempNamespace)
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("local handoff temporary namespace is not a private directory")
	}
	return root, nil
}

// pruneNativeHandoffs never scans unrelated temporary entries or follows links.
func pruneNativeHandoffs(temp string, now time.Time) {
	root := filepath.Join(temp, nativeTempNamespace)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
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
		if err != nil || info.Mode().Perm()&0o077 != 0 || !info.ModTime().Before(now.Add(-handoffMaxAge)) {
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
