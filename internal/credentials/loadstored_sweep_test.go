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

// The runtime resolves R2 credentials with Load, so the environment fallback
// works through the real wiring: with no file and the variables set, the
// store OpenDefault returns on Linux gives LoadR2Config a key.
func TestLoadR2ConfigUsesTheEnvironmentFallbackOnLinux(t *testing.T) {
	env := map[string]string{EnvR2AccessKeyID: "env-id", EnvR2SecretAccessKey: "env-secret"}
	dir := filepath.Join(t.TempDir(), CredentialsDirName)
	store, err := OpenDefault(OpenOptions{GOOS: "linux", Dir: func() (string, error) { return dir, nil }, LookupEnv: envOf(env)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Provider: ProviderR2, Bucket: "b", R2CredentialRef: testRef, R2AccountID: "0123456789abcdef0123456789abcdef"}
	awsCfg, _, err := LoadR2Config(context.Background(), cfg, store)
	if err != nil {
		t.Fatalf("LoadR2Config with only an environment key = %v", err)
	}
	got, err := awsCfg.Credentials.Retrieve(context.Background())
	if err != nil || got.AccessKeyID != "env-id" || got.SecretAccessKey != "env-secret" {
		t.Fatalf("credentials = %+v, %v", got, err)
	}
	// Without the variables there is nothing to load.
	bare, err := OpenDefault(OpenOptions{GOOS: "linux", Dir: func() (string, error) { return dir, nil }, LookupEnv: envOf(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = LoadR2Config(context.Background(), cfg, bare); !errors.Is(err, ErrCredentialFileNotFound) {
		t.Fatalf("LoadR2Config with no key anywhere = %v", err)
	}
}

// The sweep runs only in a folder Save would trust: in one open to others, or
// reached through a link, Save fails and leaves an old temporary file where it
// is (a sweep before the folder check would delete it).
func TestFileStoreSaveDoesNotSweepAFolderItRefuses(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	plant := func(dir string) string {
		t.Helper()
		path := filepath.Join(dir, ".tmp-x")
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}

	store, dir := savedFileStore(t)
	stale := plant(dir)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), "another", testCredentials); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Save into a 0755 folder = %v", err)
	}
	if _, err := os.Lstat(stale); err != nil {
		t.Errorf("Save swept a folder it refused: %v", err)
	}

	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	staleBehindLink := plant(real)
	link := filepath.Join(t.TempDir(), CredentialsDirName)
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	linked, err := NewFileStore(link)
	if err != nil {
		t.Fatal(err)
	}
	if err = linked.Save(context.Background(), "another", testCredentials); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Save through a linked folder = %v", err)
	}
	if _, err = os.Lstat(staleBehindLink); err != nil {
		t.Errorf("Save swept through a linked folder: %v", err)
	}
}

// Only files named like a temporary file are swept: a reference with "tmp" in
// it, however old its file, is a credential.
func TestFileStoreSweepKeepsAReferenceWithTmpInItsName(t *testing.T) {
	store, dir := newTestFileStore(t)
	ctx := context.Background()
	for _, ref := range []string{"my-tmp", "tmp", "a.tmp-b", "tmp-x"} {
		if err := store.Save(ctx, ref, testCredentials); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(filepath.Join(dir, ref+".json"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(ctx, "another", testCredentials); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"my-tmp", "tmp", "a.tmp-b", "tmp-x"} {
		if got, err := store.Load(ctx, ref); err != nil || got != testCredentials {
			t.Errorf("Load(%q) after a sweep = %+v, %v", ref, got, err)
		}
	}
}

// A refusal for permissions is the same set of errors whether the file or the
// folder is refused, and whichever operation meets it.
func TestInsecureRefusalsAreOneSetOfErrors(t *testing.T) {
	ctx := context.Background()
	check := func(name string, err error) {
		t.Helper()
		for _, want := range []error{ErrInsecurePermissions, ErrUnavailable, ErrCredentialFileUnreadable} {
			if !errors.Is(err, want) {
				t.Errorf("%s = %v, want it to be %v", name, err, want)
			}
		}
		if errors.Is(err, ErrMissingCredential) || errors.Is(err, ErrCredentialFileNotFound) {
			t.Errorf("%s = %v is also a missing credential", name, err)
		}
		if strings.Contains(err.Error(), "could not be read") {
			t.Errorf("%s message is prefixed twice: %v", name, err)
		}
	}
	// An insecure file.
	store, dir := savedFileStore(t)
	if err := os.Chmod(filepath.Join(dir, testRef+".json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(ctx, testRef)
	check("Load of an insecure file", err)
	// An insecure folder, on each operation that refuses it.
	if err = os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = store.Load(ctx, testRef)
	check("Load in an insecure folder", err)
	check("Save in an insecure folder", store.Save(ctx, "x", testCredentials))
	// A link where the file should be, and one where the folder should be.
	store2, dir2 := savedFileStore(t)
	if err = os.Symlink(filepath.Join(dir2, testRef+".json"), filepath.Join(dir2, "link.json")); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	_, err = store2.Load(ctx, "link")
	check("Load of a linked file", err)
	linkDir := filepath.Join(t.TempDir(), CredentialsDirName)
	if err = os.Symlink(dir2, linkDir); err != nil {
		t.Fatal(err)
	}
	linked, err := NewFileStore(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = linked.Load(ctx, testRef)
	check("Load through a linked folder", err)
	check("Delete through a linked folder", linked.Delete(ctx, testRef))
	// Not owned by this user.
	store3, _ := savedFileStore(t)
	me := store3.uid()
	store3.uid = func() int { return me + 1 }
	_, err = store3.Load(ctx, testRef)
	check("Load of what another user owns", err)
}
