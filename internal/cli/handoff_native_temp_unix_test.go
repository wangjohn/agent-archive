//go:build unix

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNativeTempNamespaceSeparatesUsersAndIgnoresLegacyRoot(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	uid := os.Geteuid()
	for _, root := range []string{filepath.Join(temp, nativeTempNamespace), nativeTempPath(temp, uid+1)} {
		must(t, os.Mkdir(root, 0o700))
		old := filepath.Join(root, "launch-old")
		must(t, os.Mkdir(old, 0o700))
		at := time.Now().Add(-8 * 24 * time.Hour)
		must(t, os.Chtimes(old, at, at))
	}
	path, err := writeNativeLaunchHandoff(temp, []byte("synthetic handoff"), time.Now())
	must(t, err)
	if filepath.Dir(filepath.Dir(path)) != nativeTempPath(temp, uid) {
		t.Fatalf("launch did not use current user's namespace: %s", path)
	}
	for _, root := range []string{filepath.Join(temp, nativeTempNamespace), nativeTempPath(temp, uid+1)} {
		_, err := os.Stat(filepath.Join(root, "launch-old"))
		must(t, err)
	}
	for path, want := range map[string]os.FileMode{nativeTempPath(temp, uid): 0o700, filepath.Dir(path): 0o700, path: 0o600} {
		info, err := os.Stat(path)
		must(t, err)
		if info.Mode().Perm() != want {
			t.Fatalf("%s permissions = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestNativeTempRejectsForeignNamespaceAndDoesNotPruneIt(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	// Inject the caller UID rather than requiring privilege to chown fixtures.
	// The fixture belongs to the actual user, not the simulated caller.
	uid := os.Geteuid() + 1
	root := nativeTempPath(temp, uid)
	must(t, os.Mkdir(root, 0o700))
	old := filepath.Join(root, "launch-old")
	must(t, os.Mkdir(old, 0o700))
	at := time.Now().Add(-8 * 24 * time.Hour)
	must(t, os.Chtimes(old, at, at))
	if _, err := nativeTempRootForUID(temp, uid); err == nil {
		t.Fatal("foreign-owned namespace accepted")
	}
	pruneNativeHandoffsForUID(temp, time.Now(), uid)
	_, err := os.Stat(old)
	must(t, err)
}

func TestNativeTempRejectsSymlinkAndPublicNamespace(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"symlink", "public"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			temp := t.TempDir()
			root := nativeTempPath(temp, os.Geteuid())
			if kind == "symlink" {
				must(t, os.Symlink(t.TempDir(), root))
			} else {
				must(t, os.Mkdir(root, 0o700))
				must(t, os.Chmod(root, 0o755))
			}
			if _, err := nativeTempRoot(temp); err == nil {
				t.Fatal("unsafe namespace accepted")
			}
		})
	}
}

type nativeTempOwnedInfo struct {
	os.FileInfo
	uid uint32
}

func (info nativeTempOwnedInfo) Sys() any { return &syscall.Stat_t{Uid: info.uid} }

func TestNativeTempDirectoryOwnershipUsesInspectedInformation(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	must(t, os.Chmod(path, 0o700))
	info, err := os.Lstat(path)
	must(t, err)
	uid := os.Geteuid()
	if privateNativeTempDirectory(nativeTempOwnedInfo{info, uint32(uid + 1)}, uid) {
		t.Fatal("foreign-owned private directory accepted")
	}
	if !privateNativeTempDirectory(nativeTempOwnedInfo{info, uint32(uid)}, uid) {
		t.Fatal("current user's private directory rejected")
	}
	// Keep cleanup's missing-root behavior harmless.
	pruneNativeHandoffs(filepath.Join(path, "missing"), time.Now())
	if _, err := os.Stat(filepath.Join(path, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup created a namespace: %v", err)
	}
}
