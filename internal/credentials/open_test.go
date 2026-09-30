package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// keychainProbe stands in for the Keychain store, so no test touches the real
// one: it records that it was asked for and never holds anything.
type keychainProbe struct{}

func (*keychainProbe) Save(context.Context, string, R2Credentials) error { return nil }

func (*keychainProbe) Load(context.Context, string) (R2Credentials, error) {
	return R2Credentials{}, ErrKeychainItemNotFound
}

func (*keychainProbe) Delete(context.Context, string) error { return nil }

func TestStoreNameAndUsesKeychain(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin":  "Keychain",
		"linux":   "credentials file",
		"freebsd": "credentials file",
		"windows": "credentials file",
		"":        "credentials file",
	} {
		if got := StoreName(goos); got != want {
			t.Errorf("StoreName(%q) = %q, want %q", goos, got, want)
		}
		if got := UsesKeychain(goos); got != (goos == "darwin") {
			t.Errorf("UsesKeychain(%q) = %v", goos, got)
		}
	}
}

func TestOpenDefaultOnMacOSIsTheKeychainAndNothingElse(t *testing.T) {
	probe := &keychainProbe{}
	var service string
	got, err := OpenDefault(OpenOptions{
		GOOS: "darwin",
		NewKeychain: func(name string) (CredentialStore, error) {
			service = name
			return probe, nil
		},
		// A Mac neither needs the data directory nor reads the environment.
		Dir: func() (string, error) {
			t.Error("the data directory was resolved for the Keychain")
			return "", errors.New("unused")
		},
		LookupEnv: func(name string) (string, bool) {
			t.Errorf("the environment was read (%s) for the Keychain", name)
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != CredentialStore(probe) {
		t.Fatalf("OpenDefault on darwin returned %T, want the Keychain store itself", got)
	}
	if service != "agent-archive" {
		t.Fatalf("Keychain service = %q, want agent-archive", service)
	}
	// Even with an environment key, the Keychain's own answer stands.
	if _, err = got.Load(context.Background(), testRef); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Fatalf("Load = %v", err)
	}

	failure := errors.New("no keychain")
	if _, err = OpenDefault(OpenOptions{GOOS: "darwin", NewKeychain: func(string) (CredentialStore, error) { return nil, failure }}); !errors.Is(err, failure) {
		t.Fatalf("OpenDefault with a failing Keychain = %v", err)
	}
}

func TestOpenDefaultKeychainConstructorIsTheRealOneByDefault(t *testing.T) {
	// The default, with no injected constructor, is NewKeychainStore: what a
	// build without a Keychain reports is its ErrUnavailable, never a nil
	// interface holding a nil store. (With a Keychain, only NewKeychainStore's
	// own tests touch it; this asks for the store and does not use it.)
	store, err := OpenDefault(OpenOptions{GOOS: "darwin"})
	if err != nil {
		if !errors.Is(err, ErrUnavailable) || store != nil {
			t.Fatalf("OpenDefault = %v, %v", store, err)
		}
		return
	}
	if store == nil {
		t.Fatal("OpenDefault returned a nil store and no error")
	}
}

func TestOpenDefaultOffMacOSIsAFileStoreWithAnEnvironmentFallback(t *testing.T) {
	for _, goos := range []string{"linux", "freebsd"} {
		dir := filepath.Join(t.TempDir(), CredentialsDirName)
		lookups := 0
		env := map[string]string{}
		store, err := OpenDefault(OpenOptions{
			GOOS: goos,
			Dir:  func() (string, error) { return dir, nil },
			LookupEnv: func(name string) (string, bool) {
				lookups++
				value, ok := env[name]
				return value, ok
			},
			NewKeychain: func(string) (CredentialStore, error) {
				t.Errorf("%s asked for the Keychain", goos)
				return nil, errors.New("unused")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		// Save goes to a file, and never reads the environment.
		if err = store.Save(ctx, testRef, testCredentials); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(dir, testRef+".json")); err != nil {
			t.Fatalf("%s: Save did not write the credentials file: %v", goos, err)
		}
		if lookups != 0 {
			t.Fatalf("%s: Save read the environment", goos)
		}

		// A reference with a file reads its file, whatever the environment says.
		env[EnvR2AccessKeyID], env[EnvR2SecretAccessKey] = "env-id", "env-secret"
		if got, err := store.Load(ctx, testRef); err != nil || got != testCredentials {
			t.Fatalf("%s: Load with a file and an environment = %+v, %v; the file must win", goos, got, err)
		}
		before := lookups

		// One without a file falls back to the environment.
		got, err := store.Load(ctx, "no-file")
		if err != nil || got.AccessKeyID != "env-id" || got.SecretAccessKey != "env-secret" {
			t.Fatalf("%s: Load with no file = %+v, %v; want the environment's key", goos, got, err)
		}
		if lookups == before {
			t.Fatalf("%s: the environment was not consulted for a reference with no file", goos)
		}

		// Delete goes to the file store alone: it removes the file, tolerates
		// a reference with no file, and never reports the environment's
		// read-only error or reads the environment.
		before = lookups
		if err = store.Delete(ctx, testRef); err != nil {
			t.Fatalf("%s: Delete = %v", goos, err)
		}
		if err = store.Delete(ctx, "no-file"); err != nil {
			t.Fatalf("%s: Delete of a reference with no file = %v", goos, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, testRef+".json")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s: Delete left the file", goos)
		}
		if lookups != before {
			t.Fatalf("%s: Delete read the environment", goos)
		}
		// The environment still answers afterwards: it was not deleted.
		if got, err = store.Load(ctx, testRef); err != nil || got.AccessKeyID != "env-id" {
			t.Fatalf("%s: Load after Delete = %+v, %v", goos, got, err)
		}
	}
}

func TestOpenDefaultDoesNotFallBackFromAFileItCannotTrust(t *testing.T) {
	dir := filepath.Join(t.TempDir(), CredentialsDirName)
	env := map[string]string{EnvR2AccessKeyID: "env-id", EnvR2SecretAccessKey: "env-secret"}
	store, err := OpenDefault(OpenOptions{GOOS: "linux", Dir: func() (string, error) { return dir, nil }, LookupEnv: envOf(env)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = store.Save(ctx, testRef, testCredentials); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, testRef+".json")

	// An insecure file is an error, not a reason to use another key.
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(ctx, testRef); !errors.Is(err, ErrInsecurePermissions) || got != (R2Credentials{}) {
		t.Fatalf("Load of an insecure file with an environment key = %+v, %v", got, err)
	}
	// A corrupt file likewise.
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"AccessKeyID":"`+testKeyID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, testRef)
	if !errors.Is(err, ErrCredentialFileUnreadable) || got != (R2Credentials{}) {
		t.Fatalf("Load of a corrupt file with an environment key = %+v, %v", got, err)
	}
	assertNoSecret(t, "corrupt file", err)
	// A folder open to others too.
	if err = os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(ctx, "no-file"); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("Load from an insecure folder with an environment key = %v", err)
	}
	// An invalid reference is not sent to the environment either.
	if err = os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(ctx, "../x"); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("Load of a bad reference = %v", err)
	}
}

func TestOpenDefaultReportsWhatIsMissingWhenNeitherHasTheKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), CredentialsDirName)
	for name, env := range map[string]map[string]string{
		"no variables":   {},
		"only a key id":  {EnvR2AccessKeyID: "env-id"},
		"only a secret":  {EnvR2SecretAccessKey: "env-secret"},
		"wrong variable": {"AWS_ACCESS_KEY_ID": "x", "AWS_SECRET_ACCESS_KEY": "y"},
	} {
		store, err := OpenDefault(OpenOptions{GOOS: "linux", Dir: func() (string, error) { return dir, nil }, LookupEnv: envOf(env)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Load(context.Background(), testRef)
		// The error names the credentials file, where setup saves a key.
		if !errors.Is(err, ErrCredentialFileNotFound) || !errors.Is(err, ErrMissingCredential) || errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: Load = %v", name, err)
		}
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("loading created the credentials folder")
	}
}

func TestOpenDefaultOffMacOSNeedsAFolder(t *testing.T) {
	if _, err := OpenDefault(OpenOptions{GOOS: "linux"}); err == nil {
		t.Error("OpenDefault with no folder succeeded")
	}
	failure := errors.New("no data directory")
	if _, err := OpenDefault(OpenOptions{GOOS: "linux", Dir: func() (string, error) { return "", failure }}); !errors.Is(err, failure) {
		t.Errorf("OpenDefault with a failing folder = %v", err)
	}
	if _, err := OpenDefault(OpenOptions{GOOS: "linux", Dir: func() (string, error) { return "relative/credentials", nil }}); err == nil {
		t.Error("OpenDefault with a relative folder succeeded")
	}
	if _, err := NewFileStore(""); err == nil {
		t.Error("NewFileStore with no folder succeeded")
	}
	if got := FileStoreDir("/data/agent-archive"); got != "/data/agent-archive/credentials" {
		t.Errorf("FileStoreDir = %q", got)
	}
}
