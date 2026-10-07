// Package providertest exercises production storage against disposable CI S3.
package providertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// DisposableS3 records actual provider requests in an isolated test prefix.
// It cannot resolve ambient credentials or contact a non-loopback endpoint.
type DisposableS3 struct {
	*storage.S3Store
	http     *disposableHTTP
	endpoint string
	bucket   string
	prefix   string
}

type disposableHTTP struct {
	client   *http.Client
	origin   string
	mu       sync.Mutex
	requests int
	statuses map[int]int
}

func (c *disposableHTTP) Do(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != c.origin {
		return nil, errors.New("acceptance request left disposable provider origin")
	}
	c.mu.Lock()
	c.requests++
	within := c.requests <= 4096
	c.mu.Unlock()
	if !within {
		return nil, errors.New("acceptance provider request budget exhausted")
	}
	response, err := c.client.Do(r)
	if response != nil {
		c.mu.Lock()
		c.statuses[response.StatusCode]++
		c.mu.Unlock()
	}
	return response, err
}

// Requests returns the number of real HTTP attempts, including provider errors.
func (s *DisposableS3) Requests() int {
	s.http.mu.Lock()
	defer s.http.mu.Unlock()
	return s.http.requests
}

// NewDisposableS3 requires explicit opt-in and a scratch bucket created by CI.
// Cleanup lists and deletes only this constructor's random prefix.
func NewDisposableS3(t *testing.T) *DisposableS3 {
	t.Helper()
	if os.Getenv("AGENT_ARCHIVE_PROVIDER_ACCEPTANCE") != "1" {
		t.Skip("disposable real-provider acceptance not requested")
	}
	endpoint := os.Getenv("AA_PROVIDER_ENDPOINT")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		t.Fatal("acceptance requires an explicit loopback disposable provider endpoint")
	}
	bucket := os.Getenv("AA_PROVIDER_BUCKET")
	access, secret := os.Getenv("AA_PROVIDER_ACCESS"), os.Getenv("AA_PROVIDER_SECRET")
	if bucket != "aa-disposable-acceptance" || access == "" || secret == "" {
		t.Fatal("acceptance scratch bucket and static credentials are required")
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	counter := &disposableHTTP{origin: endpoint, statuses: map[int]int{}, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider(access, secret, ""), HTTPClient: counter}
	prefix := "run-" + hex.EncodeToString(suffix[:])
	remote, err := storage.NewS3Store(storage.S3StoreOptions{Provider: "s3", Client: storage.NewClient(cfg, endpoint, true, 1), Bucket: bucket, Prefix: prefix, MaxGetBytes: 128 << 20})
	if err != nil {
		t.Fatal(err)
	}
	store := &DisposableS3{S3Store: remote, http: counter, endpoint: endpoint, bucket: bucket, prefix: prefix}
	t.Cleanup(func() { cleanupDisposableS3(t, store) })
	return store
}

// IndependentOwner creates a distinct credential/client identity in the same
// disposable destination. The original owner's cleanup still owns the prefix.
func (s *DisposableS3) IndependentOwner(t *testing.T) *DisposableS3 {
	t.Helper()
	access, secret := os.Getenv("AA_PROVIDER_PEER_ACCESS"), os.Getenv("AA_PROVIDER_PEER_SECRET")
	if access == "" || secret == "" || access == os.Getenv("AA_PROVIDER_ACCESS") {
		t.Fatal("a separately issued disposable provider identity is required")
	}
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider(access, secret, ""), HTTPClient: s.http}
	remote, err := storage.NewS3Store(storage.S3StoreOptions{Provider: "s3", Client: storage.NewClient(cfg, s.endpoint, true, 1), Bucket: s.bucket, Prefix: s.prefix, MaxGetBytes: 128 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return &DisposableS3{S3Store: remote, http: s.http, endpoint: s.endpoint, bucket: s.bucket, prefix: s.prefix}
}

func cleanupDisposableS3(t *testing.T, s *DisposableS3) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	objects, err := s.List(ctx, "")
	if err != nil || len(objects) > 512 {
		t.Error("disposable prefix cleanup inventory failed or exceeded bounds", err)
		return
	}
	for _, object := range objects {
		if err := s.Delete(ctx, object.Key); err != nil {
			t.Error("disposable prefix cleanup failed", err)
		}
	}
	remaining, err := s.List(ctx, "")
	if err != nil || len(remaining) != 0 {
		t.Error("disposable prefix cleanup incomplete", err)
	}
	s.http.mu.Lock()
	defer s.http.mu.Unlock()
	t.Logf("real S3 HTTP requests=%d statuses=%v; prefix cleanup verified", s.http.requests, s.http.statuses)
}
