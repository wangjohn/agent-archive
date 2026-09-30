package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// LoadStored asks what is stored, which is not what could be loaded: off
// macOS the environment answers Load for a reference with no file, and does
// not answer LoadStored. A store without a LoadStored is asked with Load.
func TestLoadStoredReadsOnlyWhatWasSaved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), CredentialsDirName)
	reads := 0
	env := map[string]string{EnvR2AccessKeyID: "env-id", EnvR2SecretAccessKey: "env-secret"}
	store, err := OpenDefault(OpenOptions{GOOS: "linux", Dir: func() (string, error) { return dir, nil }, LookupEnv: func(name string) (string, bool) {
		reads++
		value, ok := env[name]
		return value, ok
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// No file: Load answers from the environment, LoadStored says nothing is stored.
	if got, err := store.Load(ctx, testRef); err != nil || got.AccessKeyID != "env-id" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	reads = 0
	_, err = LoadStored(ctx, store, testRef)
	if !errors.Is(err, ErrCredentialFileNotFound) || !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("LoadStored with only an environment key = %v", err)
	}
	if reads != 0 {
		t.Errorf("LoadStored read the environment %d times", reads)
	}

	// A saved file is what LoadStored returns, whatever the environment says.
	if err = store.Save(ctx, testRef, testCredentials); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadStored(ctx, store, testRef); err != nil || got != testCredentials {
		t.Fatalf("LoadStored of a saved file = %+v, %v", got, err)
	}
	// An insecure file is an error there too, not "nothing stored".
	if err = os.Chmod(filepath.Join(dir, testRef+".json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadStored(ctx, store, testRef); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("LoadStored of an insecure file = %v", err)
	}

	// A store with no LoadStored (the Keychain's) is asked with Load.
	if _, err = LoadStored(ctx, &keychainProbe{}, testRef); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Fatalf("LoadStored on a plain store = %v", err)
	}
	if _, ok := any(&keychainProbe{}).(StoredLoader); ok {
		t.Fatal("the probe must not implement StoredLoader")
	}
}

// A Save removes the temporary files a killed Save left behind, once they are
// old, and only those: never a fresh one (another Save may be writing it), a
// link, a folder, a file another user owns, or anything not named like one.
func TestFileStoreSaveSweepsStaleTemporaryFiles(t *testing.T) {
	store, dir := savedFileStore(t)
	now := time.Now()
	store.now = func() time.Time { return now }
	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write(".tmp-stale", 2*time.Hour)
	write(".tmp-fresh", 10*time.Minute)
	write(".tmp-almost", staleTempAge-time.Minute)
	write("kept.json", 48*time.Hour)
	write("other-file", 48*time.Hour)
	if err := os.Mkdir(filepath.Join(dir, ".tmp-folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := write("target", 48*time.Hour)
	if err := os.Symlink(old, filepath.Join(dir, ".tmp-link")); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}

	if err := store.Save(context.Background(), "another", testCredentials); err != nil {
		t.Fatal(err)
	}
	got := entries(t, dir)
	want := []string{".tmp-almost", ".tmp-folder", ".tmp-fresh", ".tmp-link", "another.json", "kept.json", "other-file", "target", testRef + ".json"}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after Save the folder holds %v, want %v", got, want)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("the sweep removed what a link pointed to")
	}

	// Far enough in the future that everything is old: still only regular
	// files go, never a link (or what it points to) or a folder.
	store.now = func() time.Time { return now.Add(240 * time.Hour) }
	store.sweepStaleTemps()
	after := entries(t, dir)
	for _, gone := range []string{".tmp-fresh", ".tmp-almost"} {
		if strings.Contains(strings.Join(after, ","), gone) {
			t.Errorf("%s is regular and old by then, but was kept: %v", gone, after)
		}
	}
	for _, kept := range []string{".tmp-link", ".tmp-folder", "target", "kept.json"} {
		if !strings.Contains(strings.Join(after, ","), kept) {
			t.Errorf("%s was removed: %v", kept, after)
		}
	}
	store.now = func() time.Time { return now }

	// A file another user owns is left alone.
	me := store.uid()
	write(".tmp-theirs", 2*time.Hour)
	store.uid = func() int { return me + 1 }
	store.sweepStaleTemps()
	store.uid = func() int { return me }
	if _, err := os.Lstat(filepath.Join(dir, ".tmp-theirs")); err != nil {
		t.Fatal("the sweep removed a file another user owns")
	}
	// Its own user's old file goes.
	store.sweepStaleTemps()
	if _, err := os.Lstat(filepath.Join(dir, ".tmp-theirs")); err == nil {
		t.Fatal("the sweep did not remove an old file of its own user")
	}
}

// A folder refused for its permissions is one message, not the same message
// under a "could not be read" prefix; it is still both errors.
func TestFileStoreRefusalMessagesAreNotPrefixedTwice(t *testing.T) {
	store, dir := savedFileStore(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err := store.Load(ctx, testRef)
	if !errors.Is(err, ErrInsecurePermissions) || !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrCredentialFileUnreadable) {
		t.Fatalf("Load = %v, want it to be all of insecure, unavailable and unreadable", err)
	}
	if strings.Count(err.Error(), "credentials file") != 1 || strings.Contains(err.Error(), "could not be read") {
		t.Errorf("message is prefixed twice: %v", err)
	}
	if !strings.HasPrefix(err.Error(), ErrInsecurePermissions.Error()+": ") {
		t.Errorf("message = %v", err)
	}
	// The recovery advice is the insecure one, not the unreadable one.
	if action := RecoveryAction(err); !strings.Contains(action, "chmod 600") {
		t.Errorf("advice = %q", action)
	}
	if action := RecoveryActionForMessage(err.Error()); !strings.Contains(action, "chmod 600") {
		t.Errorf("advice from text = %q", action)
	}
	// A folder that is a link is refused for Delete, with the same single message.
	link := filepath.Join(t.TempDir(), CredentialsDirName)
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	linked, err := NewFileStore(link)
	if err != nil {
		t.Fatal(err)
	}
	err = linked.Delete(ctx, "x")
	if !errors.Is(err, ErrInsecurePermissions) || !errors.Is(err, ErrCredentialFileUnreadable) || strings.Contains(err.Error(), "could not be read") {
		t.Errorf("Delete through a linked folder = %v", err)
	}
}
