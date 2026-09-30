//go:build unix

package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A file is never wider than 0600 whatever the umask, and the folder is
// never wider than 0700, so nothing is readable by others even for an instant
// after creation: the temporary file is created with 0600, not created wide
// and narrowed.
func TestFileStoreSaveIsPrivateWhateverTheUmask(t *testing.T) {
	for _, umask := range []int{0, 0o022, 0o077} {
		func() {
			previous := syscall.Umask(umask)
			defer syscall.Umask(previous)
			store, dir := newTestFileStore(t)
			// The temporary file is created private: the mode it has the
			// moment it exists, before anything is written or narrowed.
			created := false
			store.tempCreated = func(path string) {
				created = true
				if got := mode(t, path); got != 0o600 {
					t.Errorf("umask %04o: temporary file created with mode %04o, want 0600", umask, got)
				}
			}
			if err := store.Save(context.Background(), testRef, testCredentials); err != nil {
				t.Fatalf("umask %04o: %v", umask, err)
			}
			if !created {
				t.Fatal("the temporary file was never observed")
			}
			if got := mode(t, dir); got != 0o700 {
				t.Errorf("umask %04o: folder mode = %04o, want 0700", umask, got)
			}
			path := filepath.Join(dir, testRef+".json")
			if got := mode(t, path); got != 0o600 {
				t.Errorf("umask %04o: file mode = %04o, want 0600", umask, got)
			}
			if got, err := store.Load(context.Background(), testRef); err != nil || got != testCredentials {
				t.Errorf("umask %04o: Load = %+v, %v", umask, got, err)
			}
			// Replacing a wider file, with the widest umask.
			if err := os.Chmod(path, 0o666); err != nil {
				t.Fatal(err)
			}
			if err := store.Save(context.Background(), testRef, testCredentials); err != nil {
				t.Fatal(err)
			}
			if got := mode(t, path); got != 0o600 {
				t.Errorf("umask %04o: replaced file mode = %04o, want 0600", umask, got)
			}
		}()
	}
}

// The file is exactly 0600 even under a umask that would narrow it further,
// so the file a later Load reads is never left unreadable to its owner.
func TestFileStoreSaveMakesTheModeExactlyPrivate(t *testing.T) {
	store, dir := newTestFileStore(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	previous := syscall.Umask(0o277)
	defer syscall.Umask(previous)
	if err := store.Save(context.Background(), testRef, testCredentials); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, filepath.Join(dir, testRef+".json")); got != 0o600 {
		t.Fatalf("file mode under umask 0277 = %04o, want 0600", got)
	}
}

// A named pipe put in place of a file must be refused, not opened: opening
// one for reading would wait for a writer.
func TestFileStoreDoesNotBlockOnANamedPipe(t *testing.T) {
	store, dir := savedFileStore(t)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.json"), 0o600); err != nil {
		t.Skipf("no named pipes: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.Load(context.Background(), "pipe")
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ErrInsecurePermissions) {
			t.Fatalf("Load of a named pipe = %v", err)
		}
		assertNoSecret(t, "named pipe", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Load blocked on a named pipe")
	}
}

// The opened file is what is trusted: a symbolic link that appears after the
// path was checked is not followed.
func TestOpenNoFollowRefusesALink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	file, err := openNoFollow(link)
	if err == nil {
		_ = file.Close()
		t.Fatal("openNoFollow followed a link")
	}
	if !isSymlinkLoop(err) {
		t.Fatalf("openNoFollow(link) = %v, want the symlink-loop error", err)
	}
}
