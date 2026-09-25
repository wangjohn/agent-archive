//go:build darwin && cgo

package credentials

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// isolateKeychainForTesting replaces every Keychain call KeychainStore makes
// with one that panics, naming the call, so a test that reaches the Keychain
// stops instead of reading, writing, or deleting a real item.
func isolateKeychainForTesting() {
	keychain = keychainAccess{
		get: func(service, account string) ([]byte, int) {
			panic(fmt.Sprintf("a test reached the real Keychain: read %s/%s", service, account))
		},
		save: func(service, account string, _ []byte) int {
			panic(fmt.Sprintf("a test reached the real Keychain: save %s/%s", service, account))
		},
		delete: func(service, account string) int {
			panic(fmt.Sprintf("a test reached the real Keychain: delete %s/%s", service, account))
		},
	}
}

// Every KeychainStore call that gets past its argument checks goes through
// the keychain seam, which TestMain made fail closed: each one stops the
// test here. A call that bypassed the seam would reach the real Keychain
// instead and fail this test by not panicking.
func TestKeychainStoreFailsClosedInTests(t *testing.T) {
	store, err := NewKeychainStore("agent-archive-test-never-used")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, call := range map[string]func(){
		"read":   func() { _, _ = store.Load(ctx, "ref") },
		"save":   func() { _ = store.Save(ctx, "ref", R2Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}) },
		"delete": func() { _ = store.Delete(ctx, "ref") },
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "a test reached the real Keychain: "+name) {
					t.Errorf("%s: got %v, want the fail-closed stand-in to stop it", name, r)
				}
			}()
			call()
		}()
	}
}
