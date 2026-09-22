package credentials

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The Keychain result-code mapping is exercised here through the Go-side
// function; no test touches a real Keychain.
func TestErrorForOSStatusDistinguishesKeychainFailures(t *testing.T) {
	if err := errorForOSStatus(osStatusSuccess); err != nil {
		t.Fatalf("success mapped to %v", err)
	}

	missing := errorForOSStatus(-25300)
	if !errors.Is(missing, ErrKeychainItemNotFound) || !errors.Is(missing, ErrMissingCredential) {
		t.Fatalf("errSecItemNotFound = %v, want a missing credential", missing)
	}
	if errors.Is(missing, ErrUnavailable) || errors.Is(missing, ErrKeychainLocked) {
		t.Fatalf("a missing item must not read as an unavailable Keychain: %v", missing)
	}

	for _, status := range []int{-25308, -25293} {
		locked := errorForOSStatus(status)
		if !errors.Is(locked, ErrKeychainLocked) || !errors.Is(locked, ErrUnavailable) {
			t.Fatalf("status %d = %v, want a locked Keychain", status, locked)
		}
		if errors.Is(locked, ErrMissingCredential) {
			t.Fatalf("status %d read as a missing credential", status)
		}
	}

	other := errorForOSStatus(-34018) // errSecMissingEntitlement
	var statusErr *KeychainStatusError
	if !errors.As(other, &statusErr) || statusErr.Status != -34018 || !errors.Is(other, ErrUnavailable) {
		t.Fatalf("other failure = %#v", other)
	}
	if errors.Is(other, ErrKeychainLocked) || errors.Is(other, ErrMissingCredential) {
		t.Fatalf("other failure was misclassified: %v", other)
	}
}

func TestRecoveryActionsAreDistinctAndSurviveWrappingAndText(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errorForOSStatus(-25300), "save it again"},
		{errorForOSStatus(-25308), "Unlock the login Keychain"},
		{errorForOSStatus(-25293), "Unlock the login Keychain"},
		{errorForOSStatus(-67671), "could not be read"},
		{ErrUnavailable, "could not be read"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		// The CLI wraps credential errors ("open storage: ..."), and status
		// sees them only as the text recorded in status.json.
		wrapped := fmt.Errorf("open storage: %w", c.err)
		action := RecoveryAction(wrapped)
		if !strings.Contains(action, c.want) {
			t.Fatalf("RecoveryAction(%v) = %q, want it to mention %q", c.err, action, c.want)
		}
		if fromText := RecoveryActionForMessage(wrapped.Error()); fromText != action {
			t.Fatalf("recorded text %q gave %q, want %q", wrapped.Error(), fromText, action)
		}
		seen[action] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected three distinct recovery actions, got %d", len(seen))
	}
	if RecoveryAction(errors.New("storage: access denied")) != "" || RecoveryActionForMessage("storage: access denied") != "" {
		t.Fatal("a non-credential failure was given credential advice")
	}
}
