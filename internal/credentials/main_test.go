package credentials

import (
	"os"
	"testing"
)

// TestMain makes the Keychain fail closed before any test runs (see
// isolateKeychainForTesting): no test but the opt-in TestKeychainRoundTrip
// can reach the developer's real login Keychain.
func TestMain(m *testing.M) {
	isolateKeychainForTesting()
	os.Exit(m.Run())
}
