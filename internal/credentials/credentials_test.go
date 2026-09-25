package credentials

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

func TestR2EndpointCanBeDerived(t *testing.T) {
	endpoint, err := R2Endpoint("", "account-123")
	if err != nil || endpoint != "https://account-123.r2.cloudflarestorage.com" {
		t.Fatalf("endpoint = %q, err = %v", endpoint, err)
	}
	for _, endpoint := range []string{
		"https://example.test/path",
		"http://example.test",
		"https://example.test?token=secret",
		"https://example.test/#fragment",
	} {
		if _, err := R2Endpoint(endpoint, ""); err == nil {
			t.Fatalf("accepted invalid endpoint %q", endpoint)
		}
	}
}

// The bucket URL Cloudflare's dashboard shows names the account in its host
// and the bucket in its path; setup takes both from it.
func TestParseR2LocationReadsTheDashboardBucketURL(t *testing.T) {
	const account = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		input string
		want  R2Location
		other bool
	}{
		{account, R2Location{AccountID: account}, false},
		{strings.ToUpper(account), R2Location{AccountID: account}, false},
		{"https://" + account + ".r2.cloudflarestorage.com", R2Location{AccountID: account}, false},
		{" https://" + strings.ToUpper(account) + ".R2.cloudflarestorage.com/my-bucket/ ", R2Location{AccountID: account, Bucket: "my-bucket"}, false},
		// Pasted without its scheme.
		{account + ".r2.cloudflarestorage.com/my-bucket", R2Location{AccountID: account, Bucket: "my-bucket"}, false},
		{"https://" + account + ".EU.r2.cloudflarestorage.com/eu-bucket", R2Location{Endpoint: "https://" + account + ".eu.r2.cloudflarestorage.com", Bucket: "eu-bucket"}, false},
		{"https://s3.example.test", R2Location{Endpoint: "https://s3.example.test"}, true},
	} {
		got, err := ParseR2Location(tc.input)
		if err != nil || got != tc.want || got.Cloudflare() == tc.other {
			t.Errorf("ParseR2Location(%q) = %+v (Cloudflare %v), %v; want %+v", tc.input, got, got.Cloudflare(), err, tc.want)
		}
	}
	for _, input := range []string{
		"",
		"account-123",
		"0123abcd",
		"http://" + account + ".r2.cloudflarestorage.com/bucket",
		"https://" + account + ".r2.cloudflarestorage.com/bucket/folder",
		"https://" + account + ".r2.cloudflarestorage.com/bucket?token=secret",
	} {
		if got, err := ParseR2Location(input); err == nil {
			t.Errorf("ParseR2Location(%q) = %+v, want an error", input, got)
		}
	}
}

func TestLoadAWSConfigHonorsExplicitProfileOverEnvironmentCredentials(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	credentialsFile := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configFile, []byte("[profile archive]\nregion = eu-west-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsFile, []byte("[archive]\naws_access_key_id = profile-key\naws_secret_access_key = profile-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
	t.Setenv("AWS_ACCESS_KEY_ID", "unrelated-env-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "unrelated-env-secret")
	cfg, err := LoadAWSConfig(context.Background(), "archive", "")
	if err != nil {
		t.Fatal(err)
	}
	value, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value.AccessKeyID != "profile-key" || value.SecretAccessKey != "profile-secret" {
		t.Fatalf("resolved unrelated credentials: %#v", value)
	}
}

func TestLoadAWSConfigDoesNotFallBackWhenSelectedProfileMissing(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	credentialsFile := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configFile, []byte("[profile other]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsFile, []byte("[other]\naws_access_key_id = other-key\naws_secret_access_key = other-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
	t.Setenv("AWS_ACCESS_KEY_ID", "unrelated-env-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "unrelated-env-secret")
	if _, err := LoadAWSConfig(context.Background(), "archive", "us-east-1"); err == nil {
		t.Fatal("selected missing profile silently fell back to environment credentials")
	}
}

func TestSecretEncodingRoundTrip(t *testing.T) {
	want := R2Credentials{AccessKeyID: "key", SecretAccessKey: "secret", SessionToken: "token"}
	data, err := EncodeSecret(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSecret(data)
	if err != nil || got != want {
		t.Fatalf("decoded = %#v, err = %v", got, err)
	}
}
