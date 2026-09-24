package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func httpStatusError(code int) error {
	return &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}},
		Err:      fmt.Errorf("status %d", code),
	}
}

func TestRetryPolicyRetriesOnlyTransientErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		attempts int
	}{
		{"access denied (403)", httpStatusError(http.StatusForbidden), 1},
		{"access denied (API code)", &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}, 1},
		{"missing bucket", &smithy.GenericAPIError{Code: "NoSuchBucket"}, 1},
		{"checksum mismatch", fmt.Errorf("%w for %q", ErrChecksumMismatch, "k"), 1},
		{"not found", ErrNotFound, 1},
		{"SDK retries exhausted", &awsretry.MaxAttemptsError{Attempt: 3, Err: httpStatusError(http.StatusServiceUnavailable)}, 1},
		{"deadline", context.DeadlineExceeded, 1},
		{"unclassified", errors.New("something else"), 1},
		{"service unavailable (503)", httpStatusError(http.StatusServiceUnavailable), 3},
		{"throttled", &smithy.GenericAPIError{Code: "SlowDown"}, 3},
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, 3},
		{"body cut short", io.ErrUnexpectedEOF, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			policy := RetryPolicy{MaxAttempts: 3, InitialWait: time.Nanosecond, MaxWait: time.Nanosecond}
			err := policy.run(context.Background(), func() error {
				attempts++
				return test.err
			})
			if !errors.Is(err, test.err) || attempts != test.attempts {
				t.Fatalf("attempts = %d, err = %v; want %d attempts", attempts, err, test.attempts)
			}
		})
	}
}

type countingGetStore struct {
	*MemoryStore
	gets int
}

func (s *countingGetStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.MemoryStore.Get(ctx, key)
}

// An existing source whose bytes differ will differ on every attempt.
func TestSourcePublicationDoesNotRetryChecksumMismatch(t *testing.T) {
	store := &countingGetStore{MemoryStore: NewMemoryStore()}
	if err := store.Put(context.Background(), "source.hash", []byte("other bytes")); err != nil {
		t.Fatal(err)
	}
	err := PutSourceThenMetadata(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), RetryPolicy{MaxAttempts: 3, InitialWait: time.Nanosecond, MaxWait: time.Nanosecond})
	if !errors.Is(err, ErrChecksumMismatch) || store.gets != 1 {
		t.Fatalf("gets = %d, err = %v", store.gets, err)
	}
}

// A server that accepts the connection and never answers must not hold a
// request (and the collector lock with it) forever.
func TestS3ClientGivesUpOnServerThatNeverResponds(t *testing.T) {
	previous := responseHeaderTimeout
	responseHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { responseHeaderTimeout = previous })

	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	// The context deadline is only a backstop for a broken test: the
	// client's own timeout must fire long before it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	_, err = store.Get(ctx, "sessions/codex/id/metadata.json")
	if err == nil || ctx.Err() != nil || time.Since(started) > 10*time.Second {
		t.Fatalf("err = %v after %s (context: %v)", err, time.Since(started), ctx.Err())
	}
	if requests.Load() == 0 {
		t.Fatal("the request never reached the server")
	}
}
