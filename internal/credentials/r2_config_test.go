package credentials

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// memoryStore is a CredentialStore in a map, standing in for the Keychain.
type memoryStore struct {
	items   map[string]R2Credentials
	loadErr error
}

func (m *memoryStore) Save(_ context.Context, ref string, value R2Credentials) error {
	m.items[ref] = value
	return nil
}

func (m *memoryStore) Load(_ context.Context, ref string) (R2Credentials, error) {
	if m.loadErr != nil {
		return R2Credentials{}, m.loadErr
	}
	value, ok := m.items[ref]
	if !ok {
		return R2Credentials{}, ErrKeychainItemNotFound
	}
	return value, nil
}

func (m *memoryStore) Delete(_ context.Context, ref string) error {
	delete(m.items, ref)
	return nil
}

// LoadR2Config builds the R2 client configuration from the stored item and
// nothing else: region auto, the endpoint from the account ID or the one
// configured, and the stored keys. Every way it can fail says why without
// the secret.
func TestLoadR2Config(t *testing.T) {
	const secret = "NEVER_PRINT_THIS_SECRET"
	good := R2Credentials{AccessKeyID: "AKID", SecretAccessKey: secret, SessionToken: "token"}
	cfg := Config{Provider: ProviderR2, Bucket: "b", R2CredentialRef: "setup-1", R2AccountID: "0123456789abcdef0123456789abcdef"}

	store := &memoryStore{items: map[string]R2Credentials{"setup-1": good}}
	awsCfg, endpoint, err := LoadR2Config(context.Background(), cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if awsCfg.Region != "auto" || endpoint != "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com" {
		t.Fatalf("region %q endpoint %q", awsCfg.Region, endpoint)
	}
	creds, err := awsCfg.Credentials.Retrieve(context.Background())
	if err != nil || creds.AccessKeyID != "AKID" || creds.SecretAccessKey != secret || creds.SessionToken != "token" {
		t.Fatalf("credentials %+v, %v", creds, err)
	}

	explicit := cfg
	explicit.R2AccountID, explicit.R2Endpoint = "", "https://custom.example.com/"
	if _, endpoint, err := LoadR2Config(context.Background(), explicit, store); err != nil || endpoint != "https://custom.example.com" {
		t.Fatalf("explicit endpoint %q, %v", endpoint, err)
	}

	noRef := cfg
	noRef.R2CredentialRef = ""
	badEndpoint := cfg
	badEndpoint.R2AccountID, badEndpoint.R2Endpoint = "", "http://insecure.example.com"
	noEndpoint := cfg
	noEndpoint.R2AccountID = ""
	for _, tc := range []struct {
		name  string
		cfg   Config
		store CredentialStore
		want  error
		text  string
	}{
		{name: "no store", cfg: cfg, store: nil, want: ErrUnavailable},
		{name: "no reference", cfg: noRef, store: store, want: ErrInvalidReference},
		{name: "item missing", cfg: cfg, store: &memoryStore{items: map[string]R2Credentials{}}, want: ErrKeychainItemNotFound},
		{name: "Keychain locked", cfg: cfg, store: &memoryStore{loadErr: ErrKeychainLocked}, want: ErrKeychainLocked},
		{name: "no secret key", cfg: cfg, store: &memoryStore{items: map[string]R2Credentials{"setup-1": {AccessKeyID: "AKID"}}}, want: ErrMissingCredential},
		{name: "no access key", cfg: cfg, store: &memoryStore{items: map[string]R2Credentials{"setup-1": {SecretAccessKey: secret}}}, want: ErrMissingCredential},
		{name: "plain http endpoint", cfg: badEndpoint, store: store, text: "invalid R2 endpoint"},
		{name: "no endpoint or account", cfg: noEndpoint, store: store, text: "account ID is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := LoadR2Config(context.Background(), tc.cfg, tc.store)
			if err == nil {
				t.Fatal("no error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
			if tc.text != "" && !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err %v, want it to say %q", err, tc.text)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("the error carries the secret")
			}
		})
	}
}

// A stored value that does not decode, or lacks a key, is reported without
// its bytes, and nothing incomplete is ever encoded for storage.
func TestSecretEncodingRefusesIncompleteValues(t *testing.T) {
	const secret = "NEVER_PRINT_THIS_SECRET"
	for _, value := range []R2Credentials{{}, {AccessKeyID: "AKID"}, {SecretAccessKey: secret}} {
		if _, err := EncodeSecret(value); !errors.Is(err, ErrMissingCredential) {
			t.Errorf("EncodeSecret(%+v) = %v", value, err)
		}
	}
	for _, data := range []string{"", "not json " + secret, `["` + secret + `"]`} {
		_, err := DecodeSecret([]byte(data))
		if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), secret) {
			t.Errorf("DecodeSecret(%q) = %v", data, err)
		}
	}
	if _, err := DecodeSecret([]byte(`{"AccessKeyID":"AKID"}`)); !errors.Is(err, ErrMissingCredential) {
		t.Errorf("a value without its secret key decoded: %v", err)
	}
}
