//go:build darwin && cgo

package credentials

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// KeychainStore checks its arguments before it asks the Keychain anything,
// so these run without touching it.
func TestKeychainStoreRefusesBeforeAskingTheKeychain(t *testing.T) {
	if _, err := NewKeychainStore(""); err == nil {
		t.Fatal("a store without a service was made")
	}
	store, err := NewKeychainStore("agent-archive-test-never-used")
	if err != nil {
		t.Fatal(err)
	}
	value := R2Credentials{AccessKeyID: "AKID", SecretAccessKey: "secret"}
	ctx := context.Background()
	if err := store.Save(ctx, "", value); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("Save with no reference: %v", err)
	}
	if err := store.Save(ctx, "ref", R2Credentials{AccessKeyID: "AKID"}); !errors.Is(err, ErrMissingCredential) {
		t.Errorf("Save of an incomplete value: %v", err)
	}
	if _, err := store.Load(ctx, ""); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("Load with no reference: %v", err)
	}
	if err := store.Delete(ctx, ""); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("Delete with no reference: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Save(cancelled, "ref", value); !errors.Is(err, context.Canceled) {
		t.Errorf("Save after cancel: %v", err)
	}
	if _, err := store.Load(cancelled, "ref"); !errors.Is(err, context.Canceled) {
		t.Errorf("Load after cancel: %v", err)
	}
	if err := store.Delete(cancelled, "ref"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete after cancel: %v", err)
	}
}

// keychainRoundTripEnv opts in to TestKeychainRoundTrip, which writes to the
// login Keychain. It never runs by default, and never in CI.
const keychainRoundTripEnv = "AGENT_ARCHIVE_KEYCHAIN_ROUND_TRIP"

// TestKeychainRoundTrip saves, loads, and deletes one item in the real login
// Keychain, under a service name of its own made for the run (never
// KeychainService), and removes it even when it fails. Run it by hand on a
// Mac with an unlocked login Keychain:
//
//	AGENT_ARCHIVE_KEYCHAIN_ROUND_TRIP=1 go test ./internal/credentials -run TestKeychainRoundTrip
func TestKeychainRoundTrip(t *testing.T) {
	if os.Getenv(keychainRoundTripEnv) != "1" {
		t.Skipf("writes to the login Keychain; set %s=1 to run it", keychainRoundTripEnv)
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	service := "agent-archive-test-" + hex.EncodeToString(suffix)
	if service == KeychainService {
		t.Fatal("the test service is the real one")
	}
	store, err := NewKeychainStore(service)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const ref = "round-trip"
	t.Cleanup(func() { _ = store.Delete(context.Background(), ref) })

	if _, err := store.Load(ctx, ref); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Fatalf("Load before Save: %v", err)
	}
	want := R2Credentials{AccessKeyID: "AKID", SecretAccessKey: "not-a-real-secret", SessionToken: "token"}
	if err := store.Save(ctx, ref, want); err != nil {
		t.Fatal(err)
	}
	// Saving again replaces the item rather than failing as a duplicate.
	want.SessionToken = "replaced"
	if err := store.Save(ctx, ref, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(ctx, ref)
	if err != nil || got != want {
		t.Fatalf("Load = %+v, %v; want %+v", got, err, want)
	}
	if err := store.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("deleting an absent item: %v", err)
	}
	if _, err := store.Load(ctx, ref); !errors.Is(err, ErrKeychainItemNotFound) {
		t.Fatalf("Load after Delete: %v", err)
	}
}
