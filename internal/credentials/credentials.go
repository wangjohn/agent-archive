// Package credentials resolves archive storage credentials without copying
// secrets into archive configuration or process arguments.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

var (
	// ErrUnavailable means no credential store can be used here: the
	// Keychain is missing (a non-macOS or cgo-disabled build) or refused
	// access, or a stored secret could not be decoded.
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

// R2Credentials are intentionally only accepted through a Keychain-backed
// reference in production setup. They are value types so callers can inject a
// test credential provider without any shell or command-line transport.
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

// Config describes one archive storage destination. For S3, AWSProfile is
// mandatory and is loaded deterministically. For R2, R2CredentialRef points
// to a Keychain item and Endpoint may be omitted when AccountID is supplied.
// The JSON tags spell the Go field names, the format already saved in users'
// config files: renaming one would make existing configs unreadable.
type Config struct {
	Provider        string `json:"Provider"`
	Bucket          string `json:"Bucket"`
	Region          string `json:"Region"`
	Prefix          string `json:"Prefix"`
	AWSProfile      string `json:"AWSProfile"`
	R2CredentialRef string `json:"R2CredentialRef"`
	R2AccountID     string `json:"R2AccountID"`
	R2Endpoint      string `json:"R2Endpoint"`
}

// Config.Provider values.
const (
	// ProviderS3 is Amazon S3, authenticated through a shared AWS profile.
	ProviderS3 = "s3"
	// ProviderR2 is Cloudflare R2, authenticated through a Keychain item.
	ProviderR2 = "r2"
)

// LoadAWSConfig loads exactly the selected shared AWS profile. Supplying an
// explicit profile makes the SDK resolve that profile's static, SSO,
// process, or role credentials; it does not fall back to unrelated environment
// credentials when the selected profile is unavailable.
func LoadAWSConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return aws.Config{}, ErrInvalidProfile
	}
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithSharedConfigProfile(profile),
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

// LoadR2Config creates an AWS config using a Keychain-resolved static
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
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" || strings.ContainsAny(accountID, "/\\ \t\r\n") {
			return "", errors.New("R2 endpoint or account ID is required")
		}
		endpoint = "https://" + accountID + ".r2.cloudflarestorage.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid R2 endpoint")
	}
	return strings.TrimRight(endpoint, "/"), nil
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

// r2DefaultHostSuffix ends the host of an account's default R2 endpoint.
const r2DefaultHostSuffix = ".r2.cloudflarestorage.com"

// ParseR2Location reads an R2 account ID, an S3 API endpoint, or the bucket
// URL Cloudflare's dashboard shows for a bucket,
// https://<account>.r2.cloudflarestorage.com/<bucket>: the account comes
// from the host and the bucket from the path.
func ParseR2Location(input string) (R2Location, error) {
	input = strings.TrimSpace(input)
	if !strings.Contains(input, "://") {
		if _, err := R2Endpoint("", input); err != nil {
			return R2Location{}, err
		}
		return R2Location{AccountID: input}, nil
	}
	u, err := url.Parse(input)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return R2Location{}, errors.New("invalid R2 endpoint")
	}
	var loc R2Location
	path := strings.Trim(u.Path, "/")
	if path != "" {
		if strings.Contains(path, "/") {
			return R2Location{}, errors.New("an R2 bucket URL names only the bucket, not a folder or object inside it")
		}
		loc.Bucket = path
	}
	host := strings.ToLower(u.Host)
	if account, ok := strings.CutSuffix(host, r2DefaultHostSuffix); ok && account != "" && !strings.Contains(account, ".") {
		loc.AccountID = account
		return loc, nil
	}
	if loc.Endpoint, err = R2Endpoint("https://"+u.Host, ""); err != nil {
		return R2Location{}, err
	}
	return loc, nil
}

// EncodeSecret is used by KeychainStore and is exported solely so a test can
// verify that the stored representation contains no JSON configuration.
func EncodeSecret(value R2Credentials) ([]byte, error) {
	if err := value.validate(); err != nil {
		return nil, err
	}
	// The secret is serialized on purpose: this is the value stored in the
	// Keychain item, and it goes nowhere else.
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
