package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// S3Store is an ObjectStore backed by Amazon S3 or a compatible endpoint such
// as Cloudflare R2. The SDK client is injected so tests can use a fake HTTP
// server without credentials or a bucket administrator account.
type S3Store struct {
	provider    string
	client      *s3.Client
	bucket      string
	prefix      string
	maxGetBytes int64
}

// S3StoreOptions configures a store. Client must be constructed with the
// desired credential provider; this package never reads credentials itself.
type S3StoreOptions struct {
	// Provider is s3 or r2 (compared case- and space-insensitively); empty is
	// unknown for custom endpoints.
	Provider string
	// Client is used as-is; endpoint, addressing style, retries, and
	// timeouts are its configuration (see NewClient).
	Client *s3.Client
	Bucket string
	Prefix string
	// MaxGetBytes bounds memory used while reading an object. Zero selects
	// the 64 MiB default appropriate for archive metadata and source bundles.
	MaxGetBytes int64
}

// NewS3Store creates a store from an AWS SDK client.
func NewS3Store(options S3StoreOptions) (*S3Store, error) {
	if options.Client == nil {
		return nil, errors.New("storage: S3 client is required")
	}
	if strings.TrimSpace(options.Bucket) == "" {
		return nil, errors.New("storage: bucket is required")
	}
	if options.Prefix != "" {
		if _, err := Prefix(options.Prefix, "archive"); err != nil {
			return nil, err
		}
	}
	maxGetBytes := options.MaxGetBytes
	if maxGetBytes <= 0 {
		maxGetBytes = 64 << 20
	}
	return &S3Store{provider: strings.ToLower(strings.TrimSpace(options.Provider)), client: options.Client, bucket: options.Bucket, prefix: strings.Trim(options.Prefix, "/"), maxGetBytes: maxGetBytes}, nil
}

// NewClient constructs an S3 client for AWS or an S3-compatible endpoint.
// For R2 callers should pass region "auto", an endpoint supplied by
// Cloudflare, and a static credentials provider from internal/credentials.
// Path style addressing is used for compatibility with both R2 and local
// fake servers.
//
// The SDK's default HTTP client waits forever for a server that accepts a
// connection and never answers (a captive portal, a stalled proxy), and a
// collector pass stuck there holds the collector lock, so every later pass
// quietly finds it busy. The client is therefore given connect, TLS, and
// response-header timeouts (see withTimeouts). A body that stalls after its
// headers arrived is bounded by the caller's context deadline instead,
// since no fixed limit suits both a small metadata read and a large upload.
func NewClient(cfg aws.Config, endpoint string, pathStyle bool, maxAttempts int) *s3.Client {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return s3.NewFromConfig(cfg, func(options *s3.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimRight(endpoint, "/"))
		}
		options.HTTPClient = withTimeouts(options.HTTPClient)
		options.UsePathStyle = pathStyle
		options.Retryer = awsretry.NewStandard(func(retryOptions *awsretry.StandardOptions) {
			retryOptions.MaxAttempts = maxAttempts
		})
	})
}

// Network timeouts for the SDK's HTTP client. Variables only so a test can
// shorten them.
var (
	dialTimeout           = 15 * time.Second
	tlsHandshakeTimeout   = 15 * time.Second
	responseHeaderTimeout = 60 * time.Second
)

// withTimeouts gives the SDK's own HTTP client (which is what
// config.LoadDefaultConfig, or no client at all, yields) connect, TLS, and
// response-header timeouts. The response-header timer starts only once the
// request body has been sent, so it does not limit upload size. A client the
// caller supplied itself is left alone.
func withTimeouts(client aws.HTTPClient) aws.HTTPClient {
	var buildable *awshttp.BuildableClient
	switch c := client.(type) {
	case nil:
		buildable = awshttp.NewBuildableClient()
	case *awshttp.BuildableClient:
		buildable = c
	default:
		return client
	}
	return buildable.
		WithDialerOptions(func(dialer *net.Dialer) { dialer.Timeout = dialTimeout }).
		WithTransportOptions(func(transport *http.Transport) {
			transport.TLSHandshakeTimeout = tlsHandshakeTimeout
			transport.ResponseHeaderTimeout = responseHeaderTimeout
		})
}

func (s *S3Store) key(relative string) (string, error) {
	return Prefix(s.prefix, relative)
}

// ObjectKey returns the full bucket key for relative, including the
// configured prefix. Keys that Prefix rejects are returned unchanged so error
// messages still identify the object.
func (s *S3Store) ObjectKey(relative string) string {
	key, err := s.key(relative)
	if err != nil {
		return relative
	}
	return key
}

// Put uploads data to relative under the store's prefix with a SHA-256
// checksum, which S3 verifies on receipt and Stat later reports.
func (s *S3Store) Put(ctx context.Context, relative string, data []byte) error {
	key, err := s.key(relative)
	if err != nil {
		return err
	}
	sum := sha256Bytes(data)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		Body:           bytes.NewReader(data),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	return err
}

// Get downloads the object at relative under the store's prefix. A missing
// object is ErrNotFound, and one larger than the store's read limit is
// ErrObjectTooLarge.
func (s *S3Store) Get(ctx context.Context, relative string) ([]byte, error) {
	key, err := s.key(relative)
	if err != nil {
		return nil, err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer output.Body.Close()
	limited := io.LimitReader(output.Body, s.maxGetBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxGetBytes {
		return nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrObjectTooLarge, relative, s.maxGetBytes)
	}
	return data, nil
}

// Stat describes an object with a HEAD request in checksum mode. Put stores
// every object with a SHA-256 checksum, and S3 reports it back as base64;
// a store that keeps none (or a multipart upload's composite checksum,
// "<digest>-<parts>") yields an empty SHA256, so callers read the object
// instead.
func (s *S3Store) Stat(ctx context.Context, relative string) (ObjectInfo, error) {
	key, err := s.key(relative)
	if err != nil {
		return ObjectInfo{}, err
	}
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		// A HEAD response has no body, so it carries no error code: a 404
		// for a missing bucket looks exactly like one for a missing object,
		// and both read as ErrNotFound here (Get, whose body names
		// NoSuchBucket, can tell them apart). For the collector the cost is
		// bounded: a metadata refresh over a bucket that is gone is recorded
		// as impossible (a refresh-skip) and not retried until the session
		// publishes again or the parser changes, and a missing bucket fails
		// every other storage call loudly anyway.
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	var info ObjectInfo
	if output.ContentLength != nil {
		info.Size = *output.ContentLength
	}
	if output.ChecksumSHA256 != nil && output.ChecksumType != types.ChecksumTypeComposite {
		if digest, err := base64.StdEncoding.DecodeString(*output.ChecksumSHA256); err == nil && len(digest) == sha256.Size {
			info.SHA256 = hex.EncodeToString(digest)
		}
	}
	return info, nil
}

// List returns every object whose key starts with relativePrefix under the
// store's prefix, following all result pages, with keys relative to the
// store's prefix and no bodies. An empty relativePrefix lists the whole
// prefix.
func (s *S3Store) List(ctx context.Context, relativePrefix string) ([]Object, error) {
	prefix, err := s.keyForList(relativePrefix)
	if err != nil {
		return nil, err
	}
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)})
	var objects []Object
	for pager.HasMorePages() {
		page, pageErr := pager.NextPage(ctx)
		if pageErr != nil {
			return nil, pageErr
		}
		for _, item := range page.Contents {
			if item.Key == nil {
				continue
			}
			obj := Object{Key: trimStorePrefix(*item.Key, s.prefix)}
			if item.Size != nil {
				obj.Size = *item.Size
			}
			if item.ETag != nil {
				obj.ETag = strings.Trim(*item.ETag, "\"")
			}
			if item.LastModified != nil {
				obj.LastModified = *item.LastModified
			}
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

func (s *S3Store) keyForList(relativePrefix string) (string, error) {
	if strings.TrimSpace(relativePrefix) == "" {
		if s.prefix == "" {
			return "", nil
		}
		return s.prefix + "/", nil
	}
	return s.key(relativePrefix)
}

// Delete removes the object at relative under the store's prefix.
func (s *S3Store) Delete(ctx context.Context, relative string) error {
	key, err := s.key(relative)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func trimStorePrefix(key, prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key
	}
	return strings.TrimPrefix(strings.TrimPrefix(key, prefix), "/")
}

// isNotFound reports whether err means the object does not exist. It trusts
// only typed evidence: the SDK's NoSuchKey or NotFound error, or an HTTP
// response whose status is 404. Error text is never matched, so a failure
// whose message merely mentions "not found" (a 403, a DNS failure, a
// misconfigured endpoint) stays an error instead of reading as a missing
// object. A 404 whose code is NoSuchBucket names a missing bucket, which is a
// configuration problem, not an absent object.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}
	var noSuchBucket *types.NoSuchBucket
	if errors.As(err, &noSuchBucket) {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket" {
		return false
	}
	var response *smithyhttp.ResponseError
	return errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound
}

func sha256Bytes(data []byte) [32]byte {
	// Kept separate from the hex helper so Put can pass the standard S3
	// base64 checksum header without a second encoding round trip.
	return sha256Sum(data)
}

var (
	_ ObjectStore   = (*S3Store)(nil)
	_ ObjectStatter = (*S3Store)(nil)
)

// NewConfiguredStore resolves the selected profile or Keychain reference and
// builds the common S3 client used for both providers. It is deliberately
// small so CLI setup can perform its synthetic round trip without knowing SDK
// credential details.
func NewConfiguredStore(ctx context.Context, cfg credentials.Config, keychain credentials.CredentialStore) (*S3Store, error) {
	var awsCfg aws.Config
	var endpoint string
	var err error
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "s3":
		region := cfg.Region
		if strings.TrimSpace(region) == "" {
			return nil, errors.New("storage: AWS region is required")
		}
		awsCfg, err = credentials.LoadAWSConfig(ctx, cfg.AWSProfile, region)
	case "r2":
		awsCfg, endpoint, err = credentials.LoadR2Config(ctx, cfg, keychain)
	default:
		return nil, fmt.Errorf("storage: unsupported provider %q", cfg.Provider)
	}
	if err != nil {
		return nil, err
	}
	client := NewClient(awsCfg, endpoint, true, 3)
	return NewS3Store(S3StoreOptions{Provider: cfg.Provider, Client: client, Bucket: cfg.Bucket, Prefix: cfg.Prefix})
}
