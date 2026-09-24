//go:build darwin && !cgo

package credentials

import "context"

// KeychainStore is unavailable in a macOS build without cgo, which cannot
// reach Security.framework. Every method fails with ErrUnavailable rather than
// shelling out to the `security` command.
type KeychainStore struct{}

// NewKeychainStore always fails with ErrUnavailable in this build.
func NewKeychainStore(service string) (*KeychainStore, error) { return nil, ErrUnavailable }

// Save always fails with ErrUnavailable in this build.
func (s *KeychainStore) Save(context.Context, string, R2Credentials) error { return ErrUnavailable }

// Load always fails with ErrUnavailable in this build.
func (s *KeychainStore) Load(context.Context, string) (R2Credentials, error) {
	return R2Credentials{}, ErrUnavailable
}

// Delete always fails with ErrUnavailable in this build.
func (s *KeychainStore) Delete(context.Context, string) error { return ErrUnavailable }
