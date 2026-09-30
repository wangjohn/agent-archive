package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The secrets these tests store. Every error any test provokes is checked to
// contain neither (assertNoSecret).
const (
	testKeyID  = "AKIATESTKEYIDNEVERSHOWN"
	testSecret = "test-secret-never-shown-in-errors"
)

var testCredentials = R2Credentials{AccessKeyID: testKeyID, SecretAccessKey: testSecret}

const testRef = "setup-0123456789abcdef0123456789abcdef"

// newTestFileStore returns a store on a folder that does not exist yet, as it
// is before the first Save, and that folder's path.
func newTestFileStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), CredentialsDirName)
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

// savedFileStore is a store that holds testCredentials under testRef.
func savedFileStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	store, dir := newTestFileStore(t)
	if err := store.Save(context.Background(), testRef, testCredentials); err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func assertNoSecret(t *testing.T, context string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range []string{testKeyID, testSecret} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error contains a secret: %v", context, err)
		}
	}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestFileStoreRoundTrip(t *testing.T) {
	store, dir := newTestFileStore(t)
	ctx := context.Background()
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewFileStore touched the file system: %v", err)
	}
	if err := store.Save(ctx, testRef, testCredentials); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, testRef)
	if err != nil || got != testCredentials {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("credentials folder mode = %04o, want 0700", got)
	}
	path := filepath.Join(dir, testRef+".json")
	if got := mode(t, path); got != 0o600 {
		t.Errorf("credentials file mode = %04o, want 0600", got)
	}
	// The file holds exactly what the Keychain item holds.
	want, err := EncodeSecret(testCredentials)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(want) {
		t.Errorf("file = %q, want the Keychain's encoding %q", data, want)
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != testRef+".json" {
		t.Errorf("credentials folder holds %v, want only the credentials file", names)
	}

	// Saving again replaces the credentials.
	next := R2Credentials{AccessKeyID: "second-id", SecretAccessKey: "second-secret", SessionToken: "token"}
	if err = store.Save(ctx, testRef, next); err != nil {
		t.Fatal(err)
	}
	if got, err = store.Load(ctx, testRef); err != nil || got != next {
		t.Fatalf("Load after replacing = %+v, %v", got, err)
	}
}

func TestFileStoreLoadOfAMissingCredentialIsAMissingCredential(t *testing.T) {
	store, dir := newTestFileStore(t)
	// Before anything is saved there is no folder.
	_, err := store.Load(context.Background(), testRef)
	if !errors.Is(err, ErrCredentialFileNotFound) || !errors.Is(err, ErrMissingCredential) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("Load with no folder = %v", err)
	}
	if _, statErr := os.Lstat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("Load created the credentials folder")
	}
	// Nor is the folder created by a probe of an absent reference.
	if err = store.Save(context.Background(), "other", testCredentials); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(context.Background(), testRef); !errors.Is(err, ErrCredentialFileNotFound) {
		t.Fatalf("Load of an absent reference = %v", err)
	}
}

func TestFileStoreLoadRefusesAFileOpenToOthers(t *testing.T) {
	for _, perm := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o606, 0o666, 0o602, 0o620, 0o610, 0o601, 0o777} {
		store, dir := savedFileStore(t)
		path := filepath.Join(dir, testRef+".json")
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		_, err := store.Load(context.Background(), testRef)
		if !errors.Is(err, ErrInsecurePermissions) || !errors.Is(err, ErrUnavailable) {
			t.Errorf("Load of a %04o file = %v, want ErrInsecurePermissions", perm, err)
			continue
		}
		assertNoSecret(t, "insecure file", err)
		// The error names the file and the exact fix.
		if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "chmod 600 '"+path+"'") {
			t.Errorf("error for a %04o file does not say how to fix it: %v", perm, err)
		}
		// Following the advice makes it load.
		if err = os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Load(context.Background(), testRef); err != nil || got != testCredentials {
			t.Errorf("Load after chmod 600 = %+v, %v", got, err)
		}
	}
}

func TestFileStoreLoadAcceptsAnOwnerOnlyFile(t *testing.T) {
	for _, perm := range []os.FileMode{0o600, 0o400, 0o700, 0o500} {
		store, dir := savedFileStore(t)
		if err := os.Chmod(filepath.Join(dir, testRef+".json"), perm); err != nil {
			t.Fatal(err)
		}
		if got, err := store.Load(context.Background(), testRef); err != nil || got != testCredentials {
			t.Errorf("Load of a %04o file = %+v, %v", perm, got, err)
		}
	}
}

func TestFileStoreRefusesAFolderOpenToOthers(t *testing.T) {
	for _, perm := range []os.FileMode{0o755, 0o750, 0o705, 0o770, 0o707, 0o777, 0o701, 0o710} {
		store, dir := savedFileStore(t)
		if err := os.Chmod(dir, perm); err != nil {
			t.Fatal(err)
		}
		before := entries(t, dir)
		_, err := store.Load(context.Background(), testRef)
		if !errors.Is(err, ErrInsecurePermissions) {
			t.Errorf("Load from a %04o folder = %v, want ErrInsecurePermissions", perm, err)
			continue
		}
		assertNoSecret(t, "insecure folder", err)
		if !strings.Contains(err.Error(), "chmod 700 '"+dir+"'") {
			t.Errorf("error for a %04o folder does not say how to fix it: %v", perm, err)
		}
		// Saving refuses too, and writes nothing into a folder others can list.
		err = store.Save(context.Background(), "another", testCredentials)
		if !errors.Is(err, ErrInsecurePermissions) {
			t.Errorf("Save into a %04o folder = %v, want ErrInsecurePermissions", perm, err)
		}
		if after := entries(t, dir); strings.Join(after, ",") != strings.Join(before, ",") {
			t.Errorf("Save into a %04o folder changed it: %v -> %v", perm, before, after)
		}
		// Deleting a secret is not the danger, so it still works.
		if err = store.Delete(context.Background(), testRef); err != nil {
			t.Errorf("Delete from a %04o folder = %v", perm, err)
		}
	}
}

func TestFileStoreRefusesASymbolicLink(t *testing.T) {
	store, dir := savedFileStore(t)
	// A private file elsewhere, linked to as the credential.
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.json")
	data, err := EncodeSecret(testCredentials)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(elsewhere, data, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.json")
	if err = os.Symlink(elsewhere, link); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	_, err = store.Load(context.Background(), "linked")
	if !errors.Is(err, ErrInsecurePermissions) || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Load through a link to a 0600 file = %v", err)
	}
	assertNoSecret(t, "symbolic link", err)

	// A link that dangles is refused as a link too, not read as missing.
	if err = os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dir, "dangling.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(context.Background(), "dangling"); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Load through a dangling link = %v", err)
	}

	// Saving over a link replaces the link and leaves what it pointed at.
	replacement := R2Credentials{AccessKeyID: "new-id", SecretAccessKey: "new-secret"}
	if err = store.Save(context.Background(), "linked", replacement); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(context.Background(), "linked"); err != nil || got != replacement {
		t.Fatalf("Load after Save over a link = %+v, %v", got, err)
	}
	if after, _ := os.ReadFile(elsewhere); string(after) != string(data) {
		t.Fatal("Save wrote through a symbolic link")
	}

	// Deleting a link removes the link, not what it points at.
	if err = os.Symlink(elsewhere, filepath.Join(dir, "again.json")); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(elsewhere); err != nil {
		t.Fatal("Delete removed the file a link pointed at")
	}
}

func TestFileStoreRefusesASymbolicLinkFolder(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), CredentialsDirName)
	if err := os.Symlink(real, dir); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, run := range map[string]func() error{
		"Load":   func() error { _, err := store.Load(ctx, testRef); return err },
		"Save":   func() error { return store.Save(ctx, testRef, testCredentials) },
		"Delete": func() error { return store.Delete(ctx, testRef) },
	} {
		err := run()
		if !errors.Is(err, ErrInsecurePermissions) || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("%s through a linked folder = %v", name, err)
		}
		assertNoSecret(t, name, err)
	}
	if names := entries(t, real); len(names) != 0 {
		t.Fatalf("something was written through the linked folder: %v", names)
	}
}

func TestFileStoreRefusesWhatIsOwnedByAnotherUser(t *testing.T) {
	store, dir := savedFileStore(t)
	ctx := context.Background()
	me := store.uid()
	// A file made by someone else cannot be made without root, so the store
	// is told it is someone else.
	store.uid = func() int { return me + 1 }
	_, err := store.Load(ctx, testRef)
	if !errors.Is(err, ErrInsecurePermissions) || !strings.Contains(err.Error(), "owned by another user") {
		t.Fatalf("Load of what another user owns = %v", err)
	}
	assertNoSecret(t, "owner", err)
	if err = store.Save(ctx, "other", testCredentials); !errors.Is(err, ErrInsecurePermissions) {
		t.Errorf("Save into a folder another user owns = %v", err)
	}
	if err = store.Delete(ctx, testRef); !errors.Is(err, ErrInsecurePermissions) {
		t.Errorf("Delete from a folder another user owns = %v", err)
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Errorf("the folder another user owns was changed: %v", names)
	}
}

// The file's owner is checked on its own, apart from the folder's: once on the
// path, and again on the opened file, which is what is trusted.
func TestFileStoreChecksTheFilesOwnerSeparately(t *testing.T) {
	store, _ := savedFileStore(t)
	me := store.uid()
	// Load asks who the user is once for the folder, once for the file's path,
	// and once for the opened file.
	for name, ownedThrough := range map[string]int{"the file's path": 1, "the opened file": 2} {
		calls := 0
		store.uid = func() int {
			calls++
			if calls <= ownedThrough {
				return me
			}
			return me + 1
		}
		_, err := store.Load(context.Background(), testRef)
		if !errors.Is(err, ErrInsecurePermissions) || !strings.Contains(err.Error(), "owned by another user") {
			t.Errorf("a file that fails the owner check at %s: Load = %v", name, err)
		}
		assertNoSecret(t, name, err)
		if calls != ownedThrough+1 {
			t.Errorf("%s: the owner was asked for %d times, want %d", name, calls, ownedThrough+1)
		}
	}
}

func TestFileStoreRefusesWhatIsNotARegularFile(t *testing.T) {
	store, dir := newTestFileStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, "seed", testCredentials); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "folder.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(ctx, "folder")
	if !errors.Is(err, ErrInsecurePermissions) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Load of a folder = %v", err)
	}
	if err = store.Delete(ctx, "folder"); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Delete of a folder = %v", err)
	}
	if info, statErr := os.Stat(filepath.Join(dir, "folder.json")); statErr != nil || !info.IsDir() {
		t.Fatal("Delete removed a folder")
	}

	// A file where the folder should be.
	notDir := filepath.Join(t.TempDir(), "credentials")
	if err = os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked, err := NewFileStore(notDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blocked.Load(ctx, "seed"); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Load with a file for a folder = %v", err)
	}
	if err = blocked.Save(ctx, "seed", testCredentials); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Save with a file for a folder = %v", err)
	}
}

func TestFileStoreSaveNeverWidensAnExistingFile(t *testing.T) {
	store, dir := savedFileStore(t)
	path := filepath.Join(dir, testRef+".json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), testRef, testCredentials); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("mode after saving over a 0644 file = %04o, want 0600", got)
	}
	if _, err := store.Load(context.Background(), testRef); err != nil {
		t.Fatalf("a file saved over a 0644 one does not load: %v", err)
	}
}

func TestFileStoreSaveFailureLeavesNoTemporaryFile(t *testing.T) {
	ctx := context.Background()

	t.Run("target is a folder", func(t *testing.T) {
		store, dir := savedFileStore(t)
		if err := os.Mkdir(filepath.Join(dir, "blocked.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		before := entries(t, dir)
		err := store.Save(ctx, "blocked", testCredentials)
		if err == nil {
			t.Fatal("Save over a folder succeeded")
		}
		assertNoSecret(t, "rename failure", err)
		if after := entries(t, dir); strings.Join(after, ",") != strings.Join(before, ",") {
			t.Fatalf("a failed Save left %v, want %v", after, before)
		}
	})

	t.Run("folder is not writable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes to a read-only folder")
		}
		store, dir := savedFileStore(t)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		err := store.Save(ctx, "another", testCredentials)
		if err == nil {
			t.Fatal("Save into a read-only folder succeeded")
		}
		assertNoSecret(t, "read-only folder", err)
		if names := entries(t, dir); len(names) != 1 || names[0] != testRef+".json" {
			t.Fatalf("a failed Save left %v", names)
		}
	})

	t.Run("credential is incomplete", func(t *testing.T) {
		store, dir := newTestFileStore(t)
		for _, value := range []R2Credentials{{}, {AccessKeyID: testKeyID}, {SecretAccessKey: testSecret}} {
			err := store.Save(ctx, testRef, value)
			if !errors.Is(err, ErrMissingCredential) {
				t.Errorf("Save(%+v) = %v", value, err)
			}
			assertNoSecret(t, "incomplete credential", err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a refused Save created the credentials folder")
		}
	})

	t.Run("context is done", func(t *testing.T) {
		store, dir := newTestFileStore(t)
		done, cancel := context.WithCancel(ctx)
		cancel()
		if err := store.Save(done, testRef, testCredentials); !errors.Is(err, context.Canceled) {
			t.Fatalf("Save with a cancelled context = %v", err)
		}
		if _, err := store.Load(done, testRef); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load with a cancelled context = %v", err)
		}
		if err := store.Delete(done, testRef); !errors.Is(err, context.Canceled) {
			t.Fatalf("Delete with a cancelled context = %v", err)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a cancelled Save created the credentials folder")
		}
	})
}

func TestFileStoreRejectsReferencesThatEscapeTheFolder(t *testing.T) {
	store, dir := savedFileStore(t)
	ctx := context.Background()
	// A file a traversing reference would reach.
	victim := filepath.Join(filepath.Dir(dir), "victim.json")
	data, err := EncodeSecret(testCredentials)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(victim, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		"", "..", ".", "../victim", "../credentials/" + testRef, "a/b", "/abs", `a\b`, "..\\victim",
		".hidden", ".tmp-0", "a\x00b", "a\nb", " a", "a ", "a b", "é", "a%2fb", "-", "_x",
		strings.Repeat("a", 129), "victim.json/..", "sub/../victim",
	} {
		if err := store.Save(ctx, ref, testCredentials); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("Save(%q) = %v, want ErrInvalidReference", ref, err)
		}
		if _, err := store.Load(ctx, ref); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("Load(%q) = %v, want ErrInvalidReference", ref, err)
		}
		if err := store.Delete(ctx, ref); !errors.Is(err, ErrInvalidReference) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidReference", ref, err)
		}
	}
	if _, err = os.Stat(victim); err != nil {
		t.Fatal("a reference reached outside the folder")
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Fatalf("a rejected reference wrote into the folder: %v", names)
	}
	// The references setup makes, and its probe, are accepted.
	for _, ref := range []string{testRef, "agent-archive-setup-check", "a.b_c-d", "0", strings.Repeat("a", 128)} {
		if err := store.Save(ctx, ref, testCredentials); err != nil {
			t.Errorf("Save(%q) = %v", ref, err)
		}
	}
}

func TestFileStoreDeleteIsIdempotent(t *testing.T) {
	store, dir := newTestFileStore(t)
	ctx := context.Background()
	// No folder at all.
	if err := store.Delete(ctx, testRef); err != nil {
		t.Fatalf("Delete with no folder = %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Delete created the credentials folder")
	}
	if err := store.Save(ctx, testRef, testCredentials); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, "keep", testCredentials); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.Delete(ctx, testRef); err != nil {
			t.Fatalf("Delete #%d = %v", i+1, err)
		}
	}
	if _, err := store.Load(ctx, testRef); !errors.Is(err, ErrCredentialFileNotFound) {
		t.Fatalf("Load after Delete = %v", err)
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "keep.json" {
		t.Fatalf("Delete removed the wrong files: %v", names)
	}
}

func TestFileStoreErrorsNeverIncludeASecret(t *testing.T) {
	store, dir := newTestFileStore(t)
	ctx := context.Background()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string, perm os.FileMode) {
		t.Helper()
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
	}
	write("notjson", "not json but it has "+testSecret+" and "+testKeyID, 0o600)
	write("nosecret", `{"AccessKeyID":"`+testKeyID+`"}`, 0o600)
	write("nokey", `{"SecretAccessKey":"`+testSecret+`"}`, 0o600)
	write("wrongtype", `{"AccessKeyID":["`+testKeyID+`"],"SecretAccessKey":"`+testSecret+`"}`, 0o600)
	write("huge", `{"AccessKeyID":"`+testKeyID+`","SecretAccessKey":"`+testSecret+`","pad":"`+strings.Repeat("x", maxCredentialFileBytes)+`"}`, 0o600)
	write("open", `{"AccessKeyID":"`+testKeyID+`","SecretAccessKey":"`+testSecret+`"}`, 0o644)
	write("empty", "", 0o600)

	for _, name := range []string{"notjson", "nosecret", "nokey", "wrongtype", "huge", "open", "empty"} {
		_, err := store.Load(ctx, name)
		if err == nil {
			t.Errorf("Load(%s) succeeded", name)
			continue
		}
		assertNoSecret(t, "Load "+name, err)
		if name == "open" {
			if !errors.Is(err, ErrInsecurePermissions) {
				t.Errorf("Load(open) = %v", err)
			}
			continue
		}
		if name == "huge" && !strings.Contains(err.Error(), "larger than a credential can be") {
			t.Errorf("Load(huge) = %v, want it refused for its size", err)
		}
		// A file that exists but is unusable is not a missing one: the
		// environment fallback must not paper over it.
		if !errors.Is(err, ErrCredentialFileUnreadable) || errors.Is(err, ErrCredentialFileNotFound) || errors.Is(err, ErrMissingCredential) {
			t.Errorf("Load(%s) = %v, want ErrCredentialFileUnreadable", name, err)
		}
	}
}
