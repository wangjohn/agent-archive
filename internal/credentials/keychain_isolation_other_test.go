//go:build !darwin || !cgo

package credentials

// isolateKeychainForTesting has nothing to do in a build without the
// Keychain: KeychainStore fails with ErrUnavailable there.
func isolateKeychainForTesting() {}
