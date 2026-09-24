//go:build darwin && cgo

package credentials

import "testing"

// The collector reads credentials in the background, where a Keychain prompt
// must never appear. The lookup query is inspected rather than run, so the
// test needs no Keychain item and never touches the user's Keychain.
func TestKeychainLookupForbidsUI(t *testing.T) {
	if !lookupForbidsUI() {
		t.Fatal("the Keychain lookup query allows authentication UI")
	}
}
