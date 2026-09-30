package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/logging"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// s3Reply is a canned S3 response: a status, headers, and an XML error body
// naming code (none when code is empty, as for a HEAD response).
type s3Reply struct {
	status int
	code   string
	header map[string]string
}

func (r s3Reply) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	for name, value := range r.header {
		w.Header().Set(name, value)
	}
	if r.code == "" {
		w.WriteHeader(r.status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(r.status)
	_, _ = fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>synthetic</Message><RequestId>r</RequestId></Error>", r.code)
}

// diagnoseClient is a real S3 client aimed at server, with one attempt per
// call so an error comes back at once.
func diagnoseClient(server *httptest.Server, provider aws.CredentialsProvider) *s3.Client {
	if provider == nil {
		provider = awscredentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")
	}
	return NewClient(aws.Config{Region: "us-east-1", Credentials: provider}, server.URL, true, 1)
}

// s3Call is one storage call a case makes through the client.
type s3Call func(context.Context, *s3.Client) error

func getObject(ctx context.Context, c *s3.Client) error {
	output, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	if err == nil {
		_ = output.Body.Close()
	}
	return err
}

func headObject(ctx context.Context, c *s3.Client) error {
	_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	return err
}

func headBucket(ctx context.Context, c *s3.Client) error {
	_, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("bucket")})
	return err
}

func listObjects(ctx context.Context, c *s3.Client) error {
	_, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bucket")})
	return err
}

// metadataCredentials is the provider the SDK picks for a profile with no
// credentials: the EC2 metadata service, here at url.
func metadataCredentials(url string) aws.CredentialsProvider {
	return ec2rolecreds.New(func(o *ec2rolecreds.Options) {
		o.Client = imds.New(imds.Options{Endpoint: url, Retryer: aws.NopRetryer{}})
	})
}

func failingCredentials(err error) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, err
	})
}

// closedURL is the address of a server that is no longer listening.
func closedURL() string {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	return server.URL
}

// Diagnose names the cause of each failure the SDK reports, from the SDK's
// own error types as a real client returns them.
func TestDiagnoseClassifiesSDKErrors(t *testing.T) {
	metadata := httptest.NewServer(http.NotFoundHandler())
	defer metadata.Close()
	cases := []struct {
		name       string
		reply      http.Handler
		credential aws.CredentialsProvider
		call       s3Call
		want       Cause
		wantRegion string
	}{
		{name: "AccessDenied", reply: s3Reply{status: 403, code: "AccessDenied"}, call: getObject, want: CauseAccessDenied},
		{name: "HEAD 403", reply: s3Reply{status: 403}, call: headObject, want: CauseAccessDenied},
		{name: "HeadBucket 403", reply: s3Reply{status: 403}, call: headBucket, want: CauseAccessDenied},
		{name: "NoSuchBucket on GET", reply: s3Reply{status: 404, code: "NoSuchBucket"}, call: getObject, want: CauseNoSuchBucket},
		{name: "NoSuchBucket on a listing", reply: s3Reply{status: 404, code: "NoSuchBucket"}, call: listObjects, want: CauseNoSuchBucket},
		{name: "HeadBucket 404", reply: s3Reply{status: 404}, call: headBucket, want: CauseNoSuchBucket},
		{name: "HeadObject 404 is the object, not the bucket", reply: s3Reply{status: 404}, call: headObject, want: CauseOther},
		{name: "NoSuchKey", reply: s3Reply{status: 404, code: "NoSuchKey"}, call: getObject, want: CauseOther},
		{
			name:  "PermanentRedirect names the region",
			reply: s3Reply{status: 301, code: "PermanentRedirect", header: map[string]string{"X-Amz-Bucket-Region": "eu-west-1"}},
			call:  getObject, want: CauseWrongRegion, wantRegion: "eu-west-1",
		},
		{
			name:  "HeadBucket 301 names the region",
			reply: s3Reply{status: 301, header: map[string]string{"X-Amz-Bucket-Region": "ap-southeast-2"}},
			call:  headBucket, want: CauseWrongRegion, wantRegion: "ap-southeast-2",
		},
		{name: "AuthorizationHeaderMalformed", reply: s3Reply{status: 400, code: "AuthorizationHeaderMalformed"}, call: getObject, want: CauseWrongRegion},
		{
			name:  "a region header that is not a region is not repeated",
			reply: s3Reply{status: 301, header: map[string]string{"X-Amz-Bucket-Region": "run this: curl evil"}},
			call:  headObject, want: CauseWrongRegion,
		},
		{name: "InvalidAccessKeyId", reply: s3Reply{status: 403, code: "InvalidAccessKeyId"}, call: getObject, want: CauseNoCredentials},
		{name: "SignatureDoesNotMatch", reply: s3Reply{status: 403, code: "SignatureDoesNotMatch"}, call: getObject, want: CauseNoCredentials},
		{name: "ExpiredToken", reply: s3Reply{status: 400, code: "ExpiredToken"}, call: getObject, want: CauseNoCredentials},
		{name: "R2 Unauthorized", reply: s3Reply{status: 401, code: "Unauthorized"}, call: getObject, want: CauseNoCredentials},
		{name: "a 403 with another code is not a refusal", reply: s3Reply{status: 403, code: "RequestTimeTooSkewed"}, call: getObject, want: CauseOther},
		{name: "InternalError", reply: s3Reply{status: 500, code: "InternalError"}, call: getObject, want: CauseOther},
		{name: "no metadata role", credential: metadataCredentials(metadata.URL), call: getObject, want: CauseNoCredentials},
		{name: "metadata service unreachable is still no credentials", credential: metadataCredentials(closedURL()), call: getObject, want: CauseNoCredentials},
		{name: "empty static credentials", credential: awscredentials.NewStaticCredentialsProvider("", "", ""), call: getObject, want: CauseNoCredentials},
		{name: "SSO sign-in expired", credential: failingCredentials(&ssocreds.InvalidTokenError{}), call: getObject, want: CauseNoCredentials},
		{name: "credential_process failed", credential: failingCredentials(&processcreds.ProviderError{Err: errors.New("exit status 1")}), call: getObject, want: CauseNoCredentials},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply := c.reply
			if reply == nil {
				reply = s3Reply{status: 200}
			}
			server := httptest.NewServer(reply)
			defer server.Close()
			err := c.call(context.Background(), diagnoseClient(server, c.credential))
			if err == nil {
				t.Fatal("call succeeded; want an error")
			}
			got := Diagnose(err)
			if got.Cause != c.want || got.Region != c.wantRegion {
				t.Fatalf("Diagnose(%v) = %q (region %q), want %q (region %q)", err, got.Cause, got.Region, c.want, c.wantRegion)
			}
			assertPlainDiagnosis(t, got)
		})
	}
}

// stsRefusal answers every STS call as an AWS query-protocol AccessDenied,
// as STS does for an AssumeRole the caller may not make.
var stsRefusal = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>synthetic</Message></Error><RequestId>r</RequestId></ErrorResponse>`)
})

// ssoNotFound answers every SSO call with a 404 ResourceNotFoundException, as
// GetRoleCredentials does for a role or account the profile names wrongly.
var ssoNotFound = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-Errortype", "ResourceNotFoundException")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"synthetic"}`)
})

// roleCredentials is a role profile's provider, with STS at url.
func roleCredentials(url string) aws.CredentialsProvider {
	client := sts.New(sts.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(url), Retryer: aws.NopRetryer{},
		Credentials: awscredentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
	})
	return stscreds.NewAssumeRoleProvider(client, "arn:aws:iam::123456789012:role/archive")
}

// ssoCredentials is an SSO profile's provider with a current sign-in, with
// the SSO portal at url.
func ssoCredentials(t *testing.T, url string) aws.CredentialsProvider {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	token := `{"accessToken":"synthetic","expiresAt":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	client := sso.New(sso.Options{Region: "us-east-1", BaseEndpoint: aws.String(url), Retryer: aws.NopRetryer{}})
	return ssocreds.New(client, "123456789012", "archive", "https://example.awsapps.com/start", func(o *ssocreds.Options) {
		o.CachedTokenFilepath = tokenFile
	})
}

// A refusal from the service that hands out a profile's credentials (STS
// for a role, SSO for a sign-in) is a credential problem: the bucket was
// never asked, so it is neither access_denied nor a missing object.
func TestDiagnoseCredentialServiceRefusals(t *testing.T) {
	for _, c := range []struct {
		name    string
		service http.Handler
		provide func(t *testing.T, url string) aws.CredentialsProvider
	}{
		{name: "STS AccessDenied", service: stsRefusal, provide: func(_ *testing.T, url string) aws.CredentialsProvider { return roleCredentials(url) }},
		{name: "SSO 404", service: ssoNotFound, provide: ssoCredentials},
	} {
		t.Run(c.name, func(t *testing.T) {
			service := httptest.NewServer(c.service)
			defer service.Close()
			bucket := httptest.NewServer(s3Reply{status: 200})
			defer bucket.Close()
			store, err := NewS3Store(S3StoreOptions{Client: diagnoseClient(bucket, c.provide(t, service.URL)), Bucket: "bucket"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Get(context.Background(), "key")
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("Get = %v; want the credential failure, not a missing object", err)
			}
			got := Diagnose(err)
			if got.Cause != CauseNoCredentials {
				t.Fatalf("Diagnose(%v) = %q, want no_credentials", err, got.Cause)
			}
			assertPlainDiagnosis(t, got)
		})
	}
}

// A credential service that cannot be reached is a network failure, like
// the storage provider itself; only the EC2 metadata service, which a Mac
// never has, means missing credentials when unreachable.
func TestDiagnoseUnreachableCredentialServiceIsNetwork(t *testing.T) {
	bucket := httptest.NewServer(s3Reply{status: 200})
	defer bucket.Close()
	err := getObject(context.Background(), diagnoseClient(bucket, roleCredentials(closedURL())))
	if got := Diagnose(err); got.Cause != CauseNetwork {
		t.Fatalf("Diagnose(%v) = %q, want network", err, got.Cause)
	}
}

// A call the caller cancelled is not the network's fault.
func TestDiagnoseCancelledIsNotNetwork(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := getObject(ctx, diagnoseClient(server, nil))
	got := Diagnose(err)
	if got.Cause != CauseOther || !strings.Contains(got.Explanation, "cancelled") {
		t.Fatalf("Diagnose(%v) = %+v, want other, naming the cancellation", err, got)
	}
	assertPlainDiagnosis(t, got)
}

// A storage provider that cannot be reached, or does not answer before the
// deadline, is a network failure.
func TestDiagnoseNetworkFailures(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		c := NewClient(aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("a", "b", "")}, closedURL(), true, 1)
		err := getObject(context.Background(), c)
		if got := Diagnose(err); got.Cause != CauseNetwork {
			t.Fatalf("Diagnose(%v) = %q, want network", err, got.Cause)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer server.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := getObject(ctx, diagnoseClient(server, nil))
		if got := Diagnose(err); got.Cause != CauseNetwork {
			t.Fatalf("Diagnose(%v) = %q, want network", err, got.Cause)
		}
	})
}

// Configuration that names missing credentials is diagnosed before any
// request: a profile that does not exist, or an R2 key not in the Keychain.
func TestDiagnoseConfiguredStoreFailures(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	if err := os.WriteFile(configFile, []byte("[profile archive]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_PROFILE", "")

	_, err := NewConfiguredStore(context.Background(), credentials.Config{Provider: "s3", Bucket: "b", Region: "us-east-1", AWSProfile: "missing"}, nil)
	if got := Diagnose(err); got.Cause != CauseNoCredentials || !strings.Contains(got.Explanation, "profile") {
		t.Errorf("missing profile: Diagnose(%v) = %+v, want no_credentials naming the profile", err, got)
	}
	_, err = NewConfiguredStore(context.Background(), credentials.Config{Provider: "r2", Bucket: "b", R2CredentialRef: "agent-archive:gone", R2AccountID: "acct123"}, fakeCredentialStore{})
	if got := Diagnose(err); got.Cause != CauseNoCredentials || !strings.Contains(got.Explanation, "Keychain") {
		t.Errorf("missing Keychain item: Diagnose(%v) = %+v, want no_credentials naming the Keychain", err, got)
	}
	for _, keychainErr := range []error{credentials.ErrKeychainLocked, &credentials.KeychainStatusError{Status: -1}, credentials.ErrUnavailable} {
		_, err = NewConfiguredStore(context.Background(), credentials.Config{Provider: "r2", Bucket: "b", R2CredentialRef: "agent-archive:r2", R2AccountID: "acct123"}, fakeCredentialStore{err: keychainErr})
		got := Diagnose(err)
		if got.Cause != CauseNoCredentials || !strings.Contains(got.Explanation, "Keychain") {
			t.Errorf("Keychain failure: Diagnose(%v) = %+v, want no_credentials naming the Keychain", err, got)
		}
		assertPlainDiagnosis(t, got)
	}
}

// The credentials file kept where there is no Keychain is diagnosed in its own
// words, told apart by the error alone, and never mentions the Keychain.
func TestDiagnoseCredentialsFileFailures(t *testing.T) {
	cfg := credentials.Config{Provider: "r2", Bucket: "b", R2CredentialRef: "setup-abc", R2AccountID: "acct123"}
	insecure := fmt.Errorf("%w: /d/credentials/setup-abc.json is accessible by other users (mode 0644); run: chmod 600 '/d/credentials/setup-abc.json'", credentials.ErrInsecurePermissions)
	unreadable := fmt.Errorf("%w: /d/credentials/setup-abc.json does not hold an access key ID and secret", credentials.ErrCredentialFileUnreadable)
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"missing":    {credentials.ErrCredentialFileNotFound, "isn't in the credentials file"},
		"insecure":   {insecure, "won't read the credentials file"},
		"unreadable": {unreadable, "couldn't be read from the credentials file"},
	} {
		_, err := NewConfiguredStore(context.Background(), cfg, fakeCredentialStore{err: tc.err})
		got := Diagnose(err)
		if got.Cause != CauseNoCredentials || !strings.Contains(got.Explanation, tc.want) {
			t.Errorf("%s: Diagnose(%v) = %+v, want no_credentials saying %q", name, err, got, tc.want)
		}
		if strings.Contains(got.Explanation, "Keychain") || strings.Contains(got.Fix, "Keychain") {
			t.Errorf("%s: the diagnosis names the Keychain: %+v", name, got)
		}
		assertPlainDiagnosis(t, got)
	}
	if got := Diagnose(insecure); !strings.Contains(got.Fix, "chmod 600") || !strings.Contains(got.Fix, "chmod 700") {
		t.Errorf("insecure fix does not say how to fix it: %+v", got)
	}
}

// A store that could not be built from its settings never reached the
// provider, so the diagnosis does not blame the provider.
func TestDiagnoseLocalSettingsDoNotBlameTheProvider(t *testing.T) {
	for _, cfg := range []credentials.Config{
		{Provider: "s3", Bucket: "b"},
		{Provider: "gcs", Bucket: "b"},
		{Provider: "r2", Bucket: "b", R2CredentialRef: "agent-archive:r2"},
	} {
		keychain := fakeCredentialStore{values: map[string]credentials.R2Credentials{"agent-archive:r2": {AccessKeyID: "a", SecretAccessKey: "b"}}}
		_, err := NewConfiguredStore(context.Background(), cfg, keychain)
		if err == nil {
			t.Fatalf("NewConfiguredStore(%+v) succeeded; want an error", cfg)
		}
		got := Diagnose(err)
		if got.Cause != CauseOther || strings.Contains(got.Explanation, "provider returned") {
			t.Errorf("Diagnose(%v) = %+v, want other, not blaming the provider", err, got)
		}
		assertPlainDiagnosis(t, got)
	}
}

// Diagnose trusts types, never words: an error whose text merely mentions a
// cause is still other.
func TestDiagnoseIgnoresErrorText(t *testing.T) {
	for _, text := range []string{"AccessDenied", "NoSuchBucket", "PermanentRedirect in the wrong region", "no EC2 IMDS role found", "dial tcp: connection refused"} {
		if got := Diagnose(errors.New(text)); got.Cause != CauseOther {
			t.Errorf("Diagnose(%q) = %q, want other", text, got.Cause)
		}
	}
	if got := Diagnose(nil); got != (Diagnosis{}) {
		t.Errorf("Diagnose(nil) = %+v, want the zero Diagnosis", got)
	}
}

// A credential_process error carries whatever the program printed, which can
// be credentials; the diagnosis never repeats it.
func TestDiagnoseNeverQuotesTheError(t *testing.T) {
	secret := "wJalrXUtnFEMI-K7MDENG-bPxRfiCYEXAMPLEKEY"
	err := fmt.Errorf("load: %w", &processcreds.ProviderError{Err: errors.New(secret)})
	got := Diagnose(err)
	if strings.Contains(got.Explanation+got.Fix, secret) {
		t.Fatalf("Diagnose repeated the error text: %+v", got)
	}
	if got.Cause != CauseNoCredentials {
		t.Fatalf("Cause = %q, want no_credentials", got.Cause)
	}
}

func assertPlainDiagnosis(t *testing.T, d Diagnosis) {
	t.Helper()
	for _, sentence := range []string{d.Explanation, d.Fix} {
		if sentence == "" || strings.Contains(sentence, "\n") || !strings.HasSuffix(sentence, ".") {
			t.Errorf("want one sentence, got %q", sentence)
		}
		if strings.Contains(sentence, "operation error") || strings.Contains(sentence, "https://") {
			t.Errorf("sentence quotes SDK text: %q", sentence)
		}
	}
}

// The SDK logs through the config's logger, which config.LoadDefaultConfig
// points at stderr. A client from NewClient logs nothing, so no "SDK <date>
// DEBUG Response has no supported checksum" line reaches the terminal.
func TestNewClientSilencesTheSDKLogger(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "body")
	}))
	defer server.Close()
	var logged bytes.Buffer
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: awscredentials.NewStaticCredentialsProvider("a", "b", ""),
		Logger:      logging.NewStandardLogger(&logged),
	}
	output, err := NewClient(cfg, server.URL, true, 1).GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("bucket"), Key: aws.String("key"), ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = output.Body.Close()
	if logged.Len() != 0 {
		t.Fatalf("SDK logged: %q", logged.String())
	}
}

// A profile with no credentials makes the SDK ask the EC2 metadata service,
// which logs "SDK <date> WARN falling back to IMDSv1" to stderr when the
// service refuses a token. NewConfiguredStore silences that, and the
// failure it leads to is no_credentials.
func TestProfileWithoutCredentialsPrintsNothing(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	if err := os.WriteFile(configFile, []byte("[profile empty]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := httptest.NewServer(http.NotFoundHandler())
	defer metadata.Close()
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", metadata.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "false")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_PROFILE", "")
	bucket := httptest.NewServer(s3Reply{status: 200})
	defer bucket.Close()

	var err error
	stderr := captureStderr(t, func() {
		store, buildErr := NewConfiguredStore(context.Background(), credentials.Config{Provider: "s3", Bucket: "b", Region: "us-east-1", AWSProfile: "empty"}, nil)
		if buildErr != nil {
			err = buildErr
			return
		}
		// The same client, aimed at the local server so nothing can reach
		// AWS even if credentials turned up.
		options := store.client.Options()
		options.BaseEndpoint = aws.String(bucket.URL)
		local, _ := NewS3Store(S3StoreOptions{Client: s3.New(options), Bucket: "b"})
		_, err = local.Get(context.Background(), "probe")
	})
	if err == nil || errors.Is(err, ErrNotFound) {
		// The metadata service's 404 is about the credentials, not the
		// object, so it must not read as a missing object.
		t.Fatalf("Get = %v; want the credential failure", err)
	}
	if got := Diagnose(err); got.Cause != CauseNoCredentials {
		t.Errorf("Diagnose(%v) = %q, want no_credentials", err, got.Cause)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- string(data)
	}()
	defer func() { os.Stderr = original }()
	fn()
	os.Stderr = original
	_ = writer.Close()
	return <-done
}
