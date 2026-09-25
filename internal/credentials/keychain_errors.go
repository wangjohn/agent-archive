package credentials

import (
	"errors"
	"fmt"
	"strings"
)

// Security.framework result codes this package distinguishes. They are
// declared here, not read through cgo, so the mapping below is testable on
// every platform; keychain_darwin.go asserts at compile time that they match
// the framework's own values.
const (
	osStatusSuccess               = 0
	osStatusItemNotFound          = -25300 // errSecItemNotFound
	osStatusInteractionNotAllowed = -25308 // errSecInteractionNotAllowed
	osStatusAuthFailed            = -25293 // errSecAuthFailed
)

// keychainSentinelError is a Keychain failure that is also an instance of a
// broader, older error, so callers that only know the broader one keep
// working: a missing item is still ErrMissingCredential, and a locked
// Keychain is still ErrUnavailable.
type keychainSentinelError struct {
	message string
	parent  error
}

func (e *keychainSentinelError) Error() string { return e.message }

func (e *keychainSentinelError) Unwrap() error { return e.parent }

var (
	// ErrKeychainItemNotFound means the Keychain holds no item for the
	// reference (errSecItemNotFound). Re-running setup stores it again.
	ErrKeychainItemNotFound error = &keychainSentinelError{message: "storage credential not found in the Keychain", parent: ErrMissingCredential}
	// ErrKeychainLocked means the Keychain is locked, or refused this
	// executable access without asking (errSecInteractionNotAllowed,
	// errSecAuthFailed). A background process never prompts, so it cannot
	// resolve this itself.
	ErrKeychainLocked error = &keychainSentinelError{message: "the Keychain is locked or denied this program access", parent: ErrUnavailable}
)

// KeychainStatusError is any other Keychain failure. It carries the
// framework's numeric result code, which identifies the failure without
// revealing anything stored, and is an ErrUnavailable.
type KeychainStatusError struct{ Status int }

// Error reports the numeric OSStatus, never any stored value.
func (e *KeychainStatusError) Error() string {
	return fmt.Sprintf("Keychain error (OSStatus %d)", e.Status)
}

// Unwrap returns ErrUnavailable, so errors.Is(err, ErrUnavailable) holds.
func (e *KeychainStatusError) Unwrap() error { return ErrUnavailable }

// errorForOSStatus maps a Security.framework result code to this package's
// errors. It is the single place the codes are interpreted.
func errorForOSStatus(status int) error {
	switch status {
	case osStatusSuccess:
		return nil
	case osStatusItemNotFound:
		return ErrKeychainItemNotFound
	case osStatusInteractionNotAllowed, osStatusAuthFailed:
		return ErrKeychainLocked
	default:
		return &KeychainStatusError{Status: status}
	}
}

// Recovery actions for credential failures, shared by status and sync so the
// same failure always gets the same advice.
const (
	recoveryMissing  = "The storage credential is missing from the Keychain. Run agent-archive setup and choose storage to save it again."
	recoveryLocked   = "The Keychain is locked or denied this program access. Unlock the login Keychain (log in, or open Keychain Access), then run agent-archive sync; if it still fails, run agent-archive setup and choose storage to grant this executable access again."
	recoveryKeychain = "The Keychain could not be read. Run agent-archive setup and choose storage to check the stored credential."
)

// RecoveryAction returns what a person should do about a credential failure,
// or "" when err is not one.
func RecoveryAction(err error) string {
	var status *KeychainStatusError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrKeychainLocked):
		return recoveryLocked
	case errors.Is(err, ErrMissingCredential):
		return recoveryMissing
	case errors.As(err, &status), errors.Is(err, ErrUnavailable):
		return recoveryKeychain
	}
	return ""
}

// RecoveryActionForMessage is RecoveryAction for an error that was persisted
// as text, as the collector's status file keeps it.
func RecoveryActionForMessage(message string) string {
	switch {
	case message == "":
		return ""
	case strings.Contains(message, ErrKeychainLocked.Error()):
		return recoveryLocked
	case strings.Contains(message, ErrKeychainItemNotFound.Error()), strings.Contains(message, ErrMissingCredential.Error()):
		return recoveryMissing
	case strings.Contains(message, "Keychain error (OSStatus"), strings.Contains(message, ErrUnavailable.Error()):
		return recoveryKeychain
	}
	return ""
}
