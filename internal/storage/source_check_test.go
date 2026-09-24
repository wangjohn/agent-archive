package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

// getOnlyStore hides MemoryStore's Stat, standing in for a store that can
// only verify an object by reading it.
type getOnlyStore struct {
	inner *MemoryStore
	gets  int
}

func (s *getOnlyStore) Put(ctx context.Context, key string, data []byte) error {
	return s.inner.Put(ctx, key, data)
}
func (s *getOnlyStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.inner.Get(ctx, key)
}
func (s *getOnlyStore) List(ctx context.Context, prefix string) ([]Object, error) {
	return s.inner.List(ctx, prefix)
}
func (s *getOnlyStore) Delete(ctx context.Context, key string) error {
	return s.inner.Delete(ctx, key)
}

// statCountingStore counts Gets on a store that can Stat.
type statCountingStore struct {
	*MemoryStore
	gets int
}

func (s *statCountingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.MemoryStore.Get(ctx, key)
}

func TestPutMetadataForSourceChecksTheRecordedSource(t *testing.T) {
	source := []byte("compressed source bytes")
	sum := SHA256Hex(source)
	cases := []struct {
		name    string
		stored  []byte // nil: missing
		sha     string
		size    int
		wantErr error
	}{
		{"matching", source, sum, len(source), nil},
		{"size unknown", source, sum, 0, nil},
		{"missing", nil, sum, len(source), ErrNotFound},
		{"different bytes", []byte("other bytes"), sum, len(source), ErrChecksumMismatch},
		{"same size, different bytes", []byte(strings.Repeat("x", len(source))), sum, len(source), ErrChecksumMismatch},
		{"different size", source, sum, len(source) + 1, ErrChecksumMismatch},
	}
	for _, statter := range []bool{true, false} {
		for _, test := range cases {
			t.Run(map[bool]string{true: "stat ", false: "get "}[statter]+test.name, func(t *testing.T) {
				memory := NewMemoryStore()
				var store ObjectStore = &statCountingStore{MemoryStore: memory}
				if !statter {
					store = &getOnlyStore{inner: memory}
				}
				if test.stored != nil {
					if err := memory.Put(context.Background(), "source", test.stored); err != nil {
						t.Fatal(err)
					}
				}
				err := PutMetadataForSource(context.Background(), store, "source", test.sha, test.size, "metadata.json", []byte(`{}`), RetryPolicy{MaxAttempts: 1})
				if !errors.Is(err, test.wantErr) || (test.wantErr == nil) != (err == nil) {
					t.Fatalf("err = %v, want %v", err, test.wantErr)
				}
				_, getErr := memory.Get(context.Background(), "metadata.json")
				if published := getErr == nil; published != (test.wantErr == nil) {
					t.Fatalf("metadata published = %v with err %v", published, err)
				}
				if counted, ok := store.(*statCountingStore); ok && counted.gets != 0 {
					t.Fatalf("a store that reports the digest was read %d times", counted.gets)
				}
			})
		}
	}
}

func TestS3StatReportsTheStoredChecksum(t *testing.T) {
	data := []byte("object bytes")
	digest := sha256.Sum256(data)
	header := base64.StdEncoding.EncodeToString(digest[:])
	for _, test := range []struct {
		name       string
		checksum   string
		checksumTy string
		status     int
		wantSHA    string
		wantErr    error
	}{
		{"full object", header, "FULL_OBJECT", http.StatusOK, SHA256Hex(data), nil},
		{"no checksum", "", "", http.StatusOK, "", nil},
		{"composite", header + "-3", "COMPOSITE", http.StatusOK, "", nil},
		{"composite with a whole digest", header, "COMPOSITE", http.StatusOK, "", nil},
		{"missing", "", "", http.StatusNotFound, "", ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					http.Error(w, "unexpected", http.StatusMethodNotAllowed)
					return
				}
				if r.Header.Get("x-amz-checksum-mode") != "ENABLED" {
					http.Error(w, "checksum mode not requested", http.StatusBadRequest)
					return
				}
				if test.checksum != "" {
					w.Header().Set("x-amz-checksum-sha256", test.checksum)
					w.Header().Set("x-amz-checksum-type", test.checksumTy)
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
			store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "archive"})
			if err != nil {
				t.Fatal(err)
			}
			info, err := store.Stat(context.Background(), "sessions/codex/id/source.x.jsonl.gz")
			if !errors.Is(err, test.wantErr) || (test.wantErr == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, test.wantErr)
			}
			if err == nil && (info.SHA256 != test.wantSHA || info.Size != int64(len(data))) {
				t.Fatalf("info = %#v, want sha %q size %d", info, test.wantSHA, len(data))
			}
		})
	}
}
