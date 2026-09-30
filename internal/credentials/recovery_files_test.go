package credentials

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The credentials file's failures get advice about the file, not the
// Keychain, whether the error is in hand (sync) or recorded as text (status).
func TestRecoveryActionsForTheCredentialsFile(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrCredentialFileNotFound, "missing from the credentials file"},
		{fmt.Errorf("%w: /d/x.json is accessible by other users (mode 0644); run: chmod 600 '/d/x.json'", ErrInsecurePermissions), "chmod 600 on the file and chmod 700 on the folder"},
		{fmt.Errorf("%w: read /d/x.json: input/output error", ErrCredentialFileUnreadable), "credentials file could not be read"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		wrapped := fmt.Errorf("open storage: %w", c.err)
		action := RecoveryAction(wrapped)
		if !strings.Contains(action, c.want) {
			t.Errorf("RecoveryAction(%v) = %q, want it to mention %q", c.err, action, c.want)
		}
		if strings.Contains(action, "Keychain") {
			t.Errorf("RecoveryAction(%v) talks of the Keychain: %q", c.err, action)
		}
		if fromText := RecoveryActionForMessage(wrapped.Error()); fromText != action {
			t.Errorf("recorded text %q gave %q, want %q", wrapped.Error(), fromText, action)
		}
		seen[action] = true
	}
	if len(seen) != len(cases) {
		t.Errorf("the credentials file's recovery actions are not distinct: %v", seen)
	}
	// The missing-file advice offers the environment too.
	if action := RecoveryAction(ErrCredentialFileNotFound); !strings.Contains(action, EnvR2AccessKeyID) || !strings.Contains(action, EnvR2SecretAccessKey) {
		t.Errorf("missing-file advice does not name the environment variables: %q", action)
	}
	// The Keychain's own advice is untouched.
	if action := RecoveryAction(ErrKeychainItemNotFound); !strings.Contains(action, "missing from the Keychain") {
		t.Errorf("Keychain advice changed: %q", action)
	}
	if RecoveryAction(errors.New("storage: access denied")) != "" {
		t.Error("a non-credential failure was given credential advice")
	}
}

func TestCredentialFileErrorsAreTheBroaderErrorsToo(t *testing.T) {
	if !errors.Is(ErrCredentialFileNotFound, ErrMissingCredential) || errors.Is(ErrCredentialFileNotFound, ErrUnavailable) {
		t.Error("ErrCredentialFileNotFound must be a missing credential and nothing else")
	}
	for _, err := range []error{ErrInsecurePermissions, ErrCredentialFileUnreadable, ErrEnvironmentReadOnly} {
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrMissingCredential) {
			t.Errorf("%v must be ErrUnavailable and not a missing credential", err)
		}
	}
	if !errors.Is(ErrEnvironmentCredentialMissing, ErrMissingCredential) {
		t.Error("a missing environment key must be a missing credential")
	}
}
