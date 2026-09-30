package credentials

import (
	"context"
	"os"
	"strings"
)

// The environment variables an R2 key can be given in: the ones setup --yes
// reads (internal/cli keeps the same two names, and a test pins that they
// agree). The secret is never taken from a command argument, which other
// processes can read. There is no session token variable, since setup --yes
// has none.
const (
	EnvR2AccessKeyID     = "AGENT_ARCHIVE_R2_ACCESS_KEY_ID"
	EnvR2SecretAccessKey = "AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY" //nolint:gosec // G101: a variable's name, not a credential.
)

var (
	// ErrEnvironmentReadOnly is what saving or deleting through the
	// environment store returns: the environment is set by whoever starts
	// agent-archive (a container's configuration, a systemd EnvironmentFile),
	// and agent-archive never writes it.
	ErrEnvironmentReadOnly error = &storeSentinelError{message: "environment credentials are read-only", parent: ErrUnavailable}
	// ErrEnvironmentCredentialMissing means the environment holds no R2 key:
	// one of the two variables is unset or empty.
	ErrEnvironmentCredentialMissing error = &storeSentinelError{message: "storage credential not set in the environment", parent: ErrMissingCredential}
)

// EnvStore is a read-only store over EnvR2AccessKeyID and
// EnvR2SecretAccessKey, for a container or a service whose credentials are
// set in its environment.
//
// The environment names one key, not many, so the reference is not looked up:
// any non-empty reference reads that key. This is deliberate. A configuration
// written by setup names a file reference, and a container is given a copy of
// the configuration whose reference has no file there; matching it against a
// fixed marker would make the environment unreachable from any configuration
// setup can produce. (The file store is consulted first, so a reference that
// has a file always reads its own key; see OpenDefault.)
type EnvStore struct {
	lookup func(string) (string, bool)
}

// NewEnvStore returns a store that reads variables through lookup, ordinarily
// os.LookupEnv; nil means that.
func NewEnvStore(lookup func(string) (string, bool)) *EnvStore {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &EnvStore{lookup: lookup}
}

// Load returns the key the environment holds, whatever the reference (which
// must not be empty), or ErrEnvironmentCredentialMissing.
func (s *EnvStore) Load(ctx context.Context, reference string) (R2Credentials, error) {
	if err := ctx.Err(); err != nil {
		return R2Credentials{}, err
	}
	if reference == "" {
		return R2Credentials{}, ErrInvalidReference
	}
	id, _ := s.lookup(EnvR2AccessKeyID)
	secret, _ := s.lookup(EnvR2SecretAccessKey)
	value := R2Credentials{AccessKeyID: strings.TrimSpace(id), SecretAccessKey: strings.TrimSpace(secret)}
	if value.validate() != nil {
		return R2Credentials{}, ErrEnvironmentCredentialMissing
	}
	return value, nil
}

// Save always fails with ErrEnvironmentReadOnly.
func (s *EnvStore) Save(context.Context, string, R2Credentials) error { return ErrEnvironmentReadOnly }

// Delete always fails with ErrEnvironmentReadOnly.
func (s *EnvStore) Delete(context.Context, string) error { return ErrEnvironmentReadOnly }

var _ CredentialStore = (*EnvStore)(nil)
