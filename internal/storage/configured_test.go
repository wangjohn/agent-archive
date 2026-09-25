package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// fakeCredentialStore is an in-memory credentials.CredentialStore.
type fakeCredentialStore struct {
	values map[string]credentials.R2Credentials
	err    error
}

func (f fakeCredentialStore) Save(context.Context, string, credentials.R2Credentials) error {
	return errors.New("not used")
}

func (f fakeCredentialStore) Load(_ context.Context, reference string) (credentials.R2Credentials, error) {
	if f.err != nil {
		return credentials.R2Credentials{}, f.err
	}
	value, ok := f.values[reference]
	if !ok {
		return credentials.R2Credentials{}, credentials.ErrMissingCredential
	}
	return value, nil
}

func (f fakeCredentialStore) Delete(context.Context, string) error { return errors.New("not used") }

// NewConfiguredStore picks credentials only from the configured source: the
// named AWS profile for S3, the Keychain reference for R2. Credentials in the
// environment, which the AWS SDK would otherwise pick up, are never used, not
// even when the configured source is missing.
func TestNewConfiguredStoreUsesOnlyTheConfiguredCredentials(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	credentialsFile := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configFile, []byte("[profile archive]\nregion = eu-west-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsFile, []byte("[archive]\naws_access_key_id = profile-key\naws_secret_access_key = profile-secret\n[default]\naws_access_key_id = default-key\naws_secret_access_key = default-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
	t.Setenv("AWS_ACCESS_KEY_ID", "env-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
	t.Setenv("AWS_SESSION_TOKEN", "env-token")
	t.Setenv("AWS_PROFILE", "default")

	keychain := fakeCredentialStore{values: map[string]credentials.R2Credentials{
		"agent-archive:r2": {AccessKeyID: "r2-key", SecretAccessKey: "r2-secret"},
	}}
	cases := []struct {
		name         string
		cfg          credentials.Config
		keychain     credentials.CredentialStore
		wantKey      string
		wantRegion   string
		wantEndpoint string
		wantErr      error // nil with wantKey == "" means any error
	}{
		{
			name:    "s3 named profile",
			cfg:     credentials.Config{Provider: "s3", Bucket: "b", Region: "us-east-2", Prefix: "agent-archive/", AWSProfile: "archive"},
			wantKey: "profile-key", wantRegion: "us-east-2",
		},
		{
			name:    "s3 provider spelled loosely",
			cfg:     credentials.Config{Provider: " S3 ", Bucket: "b", Region: "us-east-2", AWSProfile: "archive"},
			wantKey: "profile-key", wantRegion: "us-east-2",
		},
		{
			name: "s3 missing profile does not fall back to the environment",
			cfg:  credentials.Config{Provider: "s3", Bucket: "b", Region: "us-east-1", AWSProfile: "missing"},
		},
		{
			name:    "s3 empty profile does not mean the default chain",
			cfg:     credentials.Config{Provider: "s3", Bucket: "b", Region: "us-east-1"},
			wantErr: credentials.ErrInvalidProfile,
		},
		{
			name: "s3 without a region",
			cfg:  credentials.Config{Provider: "s3", Bucket: "b", AWSProfile: "archive"},
		},
		{
			name:     "r2 keychain reference",
			cfg:      credentials.Config{Provider: "r2", Bucket: "b", R2CredentialRef: "agent-archive:r2", R2AccountID: "acct123"},
			keychain: keychain,
			wantKey:  "r2-key", wantRegion: "auto", wantEndpoint: "https://acct123.r2.cloudflarestorage.com",
		},
		{
			name:     "r2 missing keychain item does not fall back to the environment",
			cfg:      credentials.Config{Provider: "r2", Bucket: "b", R2CredentialRef: "agent-archive:gone", R2AccountID: "acct123"},
			keychain: keychain,
			wantErr:  credentials.ErrMissingCredential,
		},
		{
			name:     "r2 keychain unavailable",
			cfg:      credentials.Config{Provider: "r2", Bucket: "b", R2CredentialRef: "agent-archive:r2", R2AccountID: "acct123"},
			keychain: fakeCredentialStore{err: credentials.ErrUnavailable},
			wantErr:  credentials.ErrUnavailable,
		},
		{
			name:    "r2 without a credential store",
			cfg:     credentials.Config{Provider: "r2", Bucket: "b", R2CredentialRef: "agent-archive:r2", R2AccountID: "acct123"},
			wantErr: credentials.ErrUnavailable,
		},
		{
			name:     "r2 without a reference",
			cfg:      credentials.Config{Provider: "r2", Bucket: "b", R2AccountID: "acct123"},
			keychain: keychain,
			wantErr:  credentials.ErrInvalidReference,
		},
		{
			name: "unknown provider",
			cfg:  credentials.Config{Provider: "gcs", Bucket: "b", AWSProfile: "archive", Region: "us-east-1"},
		},
		{
			name:     "missing bucket",
			cfg:      credentials.Config{Provider: "r2", R2CredentialRef: "agent-archive:r2", R2AccountID: "acct123"},
			keychain: keychain,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, err := NewConfiguredStore(context.Background(), c.cfg, c.keychain)
			if c.wantKey == "" {
				if err == nil {
					t.Fatalf("NewConfiguredStore succeeded; want an error")
				}
				if c.wantErr != nil && !errors.Is(err, c.wantErr) {
					t.Fatalf("error = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			options := store.client.Options()
			value, err := options.Credentials.Retrieve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if value.AccessKeyID != c.wantKey || value.SessionToken == "env-token" {
				t.Fatalf("credentials = %q (session token %q), want %q from the configured source", value.AccessKeyID, value.SessionToken, c.wantKey)
			}
			if options.Region != c.wantRegion {
				t.Errorf("region = %q, want %q", options.Region, c.wantRegion)
			}
			if got := aws.ToString(options.BaseEndpoint); got != c.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", got, c.wantEndpoint)
			}
			if !options.UsePathStyle {
				t.Error("path-style addressing is off")
			}
			if wantPrefix := strings.Trim(c.cfg.Prefix, "/"); store.bucket != c.cfg.Bucket || store.prefix != wantPrefix {
				t.Errorf("bucket/prefix = %q/%q, want %q/%q", store.bucket, store.prefix, c.cfg.Bucket, wantPrefix)
			}
		})
	}
}
