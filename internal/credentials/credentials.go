// Package credentials resolves archive storage credentials without copying
// secrets into archive configuration or process arguments.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go/logging"
	"github.com/wangjohn/agent-archive/internal/destination"
)

var (
	// ErrUnavailable means no credential store can be used here: the
	// Keychain is missing (a macOS build without cgo) or refused access, the
	// credentials file is insecure or unreadable (ErrInsecurePermissions,
	// ErrCredentialFileUnreadable), or a stored secret could not be decoded.
	ErrUnavailable = errors.New("credential store unavailable")
	// ErrInvalidReference means a credential reference is empty.
	ErrInvalidReference = errors.New("invalid credential reference")
	// ErrInvalidProfile means no AWS profile was named.
	ErrInvalidProfile = errors.New("invalid AWS profile")
	// ErrMissingCredential means a stored credential lacks its access key ID
	// or secret access key.
	ErrMissingCredential = errors.New("credential is missing")
)

// KeychainService is the canonical macOS Keychain service name this archive
// uses for every R2CredentialRef, so a hook, the collector, and setup all
// resolve the same stored item.

const KeychainService = "agent-archive"

// R2Credentials are intentionally only accepted through a reference to a
// credential store (the Keychain on macOS, a private file elsewhere; see
// OpenDefault) in production setup. They are value types so callers can
// inject a test credential provider without any shell or command-line transport.
// The JSON tags pin the format already stored in users' Keychains: renaming
// one would make existing credentials unreadable.

type R2Credentials struct {
	AccessKeyID     string `json:"AccessKeyID"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
}

func (c R2Credentials) validate() error {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return ErrMissingCredential
	}
	return nil
}

// CredentialStore stores and resolves opaque references. Implementations must
// never include secret values in errors or diagnostic output.

type CredentialStore interface {
	Save(ctx context.Context, reference string, value R2Credentials) error
	Load(ctx context.Context, reference string) (R2Credentials, error)
	Delete(ctx context.Context, reference string) error
}

// Config is the persisted pure destination value.
type Config = destination.Config

// ProviderS3 identifies a destination using the selected AWS profile.
const ProviderS3 = destination.ProviderS3

// ProviderR2 identifies a destination using stored R2 credentials.
const ProviderR2 = destination.ProviderR2

// LoadAWSConfig loads exactly the selected shared AWS profile. Supplying an
// explicit profile makes the SDK resolve that profile's static, SSO,
// process, or role credentials; it does not fall back to unrelated environment
// credentials when the selected profile is unavailable.
//
// The SDK's logger is silenced. The clients the SDK builds while loading
// (the EC2 metadata client that looks for credentials when a profile has
// none, SSO, STS) keep the logger they were built with, and the SDK's
// default prints to stderr, such as "SDK <date> WARN falling back to
// IMDSv1" when a profile has no credentials. Failures still reach the
// caller as errors.

func LoadAWSConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return aws.Config{}, ErrInvalidProfile
	}
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithSharedConfigProfile(profile),
		awsconfig.WithLogger(logging.Nop{}),
	}
	if strings.TrimSpace(region) != "" {
		options = append(options, awsconfig.WithRegion(strings.TrimSpace(region)))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS profile %q: %w", profile, err)
	}
	return cfg, nil
}

// LoadR2Config creates an AWS config using a store-resolved static
// provider. Region is always "auto", as required by Cloudflare R2. Endpoint
// is normalized and may be derived from a Cloudflare account ID.

func LoadR2Config(ctx context.Context, cfg Config, store CredentialStore) (aws.Config, string, error) {
	if store == nil {
		return aws.Config{}, "", ErrUnavailable
	}
	if cfg.R2CredentialRef == "" {
		return aws.Config{}, "", ErrInvalidReference
	}
	value, err := store.Load(ctx, cfg.R2CredentialRef)
	if err != nil {
		return aws.Config{}, "", err
	}
	if err := value.validate(); err != nil {
		return aws.Config{}, "", err
	}
	endpoint, err := R2Endpoint(cfg.R2Endpoint, cfg.R2AccountID)
	if err != nil {
		return aws.Config{}, "", err
	}
	provider := awscredentials.NewStaticCredentialsProvider(value.AccessKeyID, value.SecretAccessKey, value.SessionToken)
	return aws.Config{Region: "auto", Credentials: provider}, endpoint, nil
}

// R2Endpoint returns a validated endpoint. Cloudflare's account endpoint is
// inferred only when the caller explicitly supplies an account ID.

func R2Endpoint(endpoint, accountID string) (string, error) {
	return destination.R2Endpoint(endpoint, accountID)
}

// R2Location is what an R2 account ID or URL pasted into setup names.

type R2Location struct {
	// AccountID is set when the input is an account ID, or a URL on the
	// account's default endpoint.
	AccountID string
	// Endpoint is the normalized endpoint of any other URL, such as a
	// jurisdiction's (https://<account>.eu.r2.cloudflarestorage.com).
	Endpoint string
	// Bucket is the bucket a URL's path names, or "".
	Bucket string
}

// r2HostSuffix ends the host of every Cloudflare R2 endpoint.

const r2HostSuffix = ".r2.cloudflarestorage.com"

// r2AccountID is the shape of a Cloudflare account ID.

var r2AccountID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Cloudflare reports whether l is on a Cloudflare R2 host. Any other https
// endpoint is accepted, as an S3-compatible service, but is worth a warning.

func (l R2Location) Cloudflare() bool {
	return l.Endpoint == "" || strings.HasSuffix(l.Endpoint, r2HostSuffix)
}

// ParseR2Location reads an R2 account ID, an S3 API endpoint, or the bucket
// URL Cloudflare's dashboard shows for a bucket,
// https://<account>.r2.cloudflarestorage.com/<bucket>: the account comes
// from the host and the bucket from the path. A Cloudflare host without its
// https:// is read as a URL.

func ParseR2Location(input string) (R2Location, error) {
	input = strings.TrimSpace(input)
	if !strings.Contains(input, "://") && strings.Contains(strings.ToLower(input), r2HostSuffix) {
		input = "https://" + input
	}
	if !strings.Contains(input, "://") {
		account := strings.ToLower(input)
		if !r2AccountID.MatchString(account) {
			return R2Location{}, errors.New("an R2 account ID is 32 characters, 0-9 and a-f")
		}
		return R2Location{AccountID: account}, nil
	}
	u, err := url.Parse(input)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return R2Location{}, errors.New("invalid R2 endpoint")
	}
	bucket := strings.Trim(u.Path, "/")
	if strings.Contains(bucket, "/") {
		return R2Location{}, errors.New("an R2 bucket URL names only the bucket, not a folder or object inside it")
	}
	host := strings.ToLower(u.Host)
	if account, ok := strings.CutSuffix(host, r2HostSuffix); ok && r2AccountID.MatchString(account) {
		return R2Location{AccountID: account, Bucket: bucket}, nil
	}
	endpoint, err := R2Endpoint("https://"+host, "")
	if err != nil {
		return R2Location{}, err
	}
	return R2Location{Endpoint: endpoint, Bucket: bucket}, nil
}

// EncodeSecret is used by KeychainStore and FileStore and is exported solely so a test can
// verify that the stored representation contains no JSON configuration.

func EncodeSecret(value R2Credentials) ([]byte, error) {
	if err := value.validate(); err != nil {
		return nil, err
	}
	// The secret is serialized on purpose: this is the value stored in the
	// Keychain item or the credentials file, and it goes nowhere else.
	return json.Marshal(value) //nolint:gosec // G117: see above.
}

// DecodeSecret parses a value EncodeSecret produced. Any decoding failure is
// ErrUnavailable, so the stored bytes never appear in an error.

func DecodeSecret(data []byte) (R2Credentials, error) {
	var value R2Credentials
	if err := json.Unmarshal(data, &value); err != nil {
		return R2Credentials{}, ErrUnavailable
	}
	if err := value.validate(); err != nil {
		return R2Credentials{}, err
	}
	return value, nil
}
