package credentials

import (
	"context"
	"errors"
	"os"

	"github.com/wangjohn/agent-archive/internal/platform"
)

// UsesKeychain reports whether system keeps R2 credentials in the Keychain
// (macOS) rather than in a private file. Only Darwin does: an Unknown
// system does not, and OpenDefault does not open a file store for it either.
func UsesKeychain(system platform.OS) bool { return system == platform.Darwin }

// StoreName is how a message names where R2 credentials are kept on system:
// "Keychain" on macOS, "credentials file" on Linux. It is a noun without an
// article, so a caller writes "the "+StoreName(system) or capitalizes it, and
// macOS keeps the wording it always had. An Unknown system claims neither:
// "credential store".
func StoreName(system platform.OS) string {
	switch system {
	case platform.Darwin:
		return "Keychain"
	case platform.Linux:
		return "credentials file"
	case platform.Unknown:
	}
	return "credential store"
}

// ErrUnsupportedPlatform means OpenDefault was asked for the credential store
// of an operating system it does not know. It refuses rather than guess: a
// wrong guess would put R2 secrets in the wrong place (a file where the
// system has a keychain).
var ErrUnsupportedPlatform = errors.New("credentials are not supported on this operating system")

// OpenOptions are OpenDefault's inputs. Everything that reaches the real
// system is injectable, so both platforms' choices are tested on any OS.
type OpenOptions struct {
	// OS is the platform to open the store for: platform.Current in
	// production. An Unknown one is ErrUnsupportedPlatform.
	OS platform.OS
	// Dir returns the folder of credential files (FileStoreDir of the data
	// directory). It is called only off macOS, so a Mac never needs the data
	// directory to open its Keychain.
	Dir func() (string, error)
	// LookupEnv reads the environment for the read-only fallback. Nil means
	// os.LookupEnv.
	LookupEnv func(string) (string, bool)
	// NewKeychain opens the Keychain store for a service. Nil means
	// NewKeychainStore.
	NewKeychain func(service string) (CredentialStore, error)
}

// OpenDefault opens the credential store for a platform.
//
// On macOS it is the Keychain store under KeychainService, exactly as before.
//
// On Linux (and any system added with a file store) it is the file store in
// Dir(), with the environment
// (EnvStore) as a read-only fallback behind it. The fallback is part of the
// one store rather than a separate choice because a caller cannot know which
// one holds a key: a configuration made by setup names a reference, and a
// container has that configuration but the key only in its environment.
//   - Load reads the file first. A reference that has a file always reads
//     it, never the environment. Only a missing file (ErrCredentialFileNotFound)
//     falls back to the environment; a file that exists but is insecure or
//     unreadable is an error, since silently using another key would hide it.
//   - Save and Delete go to the file store alone. The environment store is
//     held as a read-only Loader, so they cannot reach it.
func OpenDefault(opts OpenOptions) (CredentialStore, error) {
	if opts.OS != platform.Darwin && opts.OS != platform.Linux {
		return nil, ErrUnsupportedPlatform
	}
	if UsesKeychain(opts.OS) {
		open := opts.NewKeychain
		if open == nil {
			open = openKeychainStore
		}
		return open(KeychainService)
	}
	if opts.Dir == nil {
		return nil, errors.New("credentials folder is not known")
	}
	dir, err := opts.Dir()
	if err != nil {
		return nil, err
	}
	files, err := NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &fileThenEnvStore{files: files, env: NewEnvStore(lookup)}, nil
}

// openKeychainStore is NewKeychainStore as an interface, never a non-nil
// interface holding a nil store.
func openKeychainStore(service string) (CredentialStore, error) {
	store, err := NewKeychainStore(service)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// envLoader is all the fallback can do; it has no Save or Delete to call.
type envLoader interface {
	Load(ctx context.Context, reference string) (R2Credentials, error)
}

// StoredLoader is implemented by a store whose Load can answer from somewhere
// setup did not save to, so that "is a credential actually stored for this
// reference?" is a different question from "can a key be loaded for it?".
// LoadStored answers the first: it reads only what Save writes. Runtime
// paths (the collector, hooks, sync, status) use Load, with its fallback;
// setup and its checks use LoadStored, so they never offer to keep, or count
// as stored, a key that a scheduled collector, which does not inherit an
// interactive shell's environment, would not find.
type StoredLoader interface {
	LoadStored(ctx context.Context, reference string) (R2Credentials, error)
}

// LoadStored reads what was saved under reference and nothing else: the
// store's LoadStored when it has one, otherwise its Load (the Keychain's Load
// already reads only the Keychain).
func LoadStored(ctx context.Context, store CredentialStore, reference string) (R2Credentials, error) {
	if stored, ok := store.(StoredLoader); ok {
		return stored.LoadStored(ctx, reference)
	}
	return store.Load(ctx, reference)
}

// fileThenEnvStore is OpenDefault's store off macOS.
type fileThenEnvStore struct {
	files *FileStore
	env   envLoader
}

func (s *fileThenEnvStore) Save(ctx context.Context, reference string, value R2Credentials) error {
	return s.files.Save(ctx, reference, value)
}

func (s *fileThenEnvStore) Delete(ctx context.Context, reference string) error {
	return s.files.Delete(ctx, reference)
}

func (s *fileThenEnvStore) Load(ctx context.Context, reference string) (R2Credentials, error) {
	value, err := s.files.Load(ctx, reference)
	if !errors.Is(err, ErrCredentialFileNotFound) {
		return value, err
	}
	if fromEnv, envErr := s.env.Load(ctx, reference); envErr == nil {
		return fromEnv, nil
	}
	// Neither holds it: the error names the file, where setup saves a key.
	return R2Credentials{}, err
}

// LoadStored reads the credentials file alone: never the environment.
func (s *fileThenEnvStore) LoadStored(ctx context.Context, reference string) (R2Credentials, error) {
	return s.files.Load(ctx, reference)
}

var (
	_ CredentialStore = (*fileThenEnvStore)(nil)
	_ StoredLoader    = (*fileThenEnvStore)(nil)
)
