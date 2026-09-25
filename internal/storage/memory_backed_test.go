package storage_test

// Tests of the store helpers that run against the in-memory store, which
// lives in storagetest (test code only) and so can only be used from this
// external test package.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func responseError(status int, cause error) error {
	return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      cause,
	}}
}

// getOnlyStore hides MemoryStore's Stat, standing in for a store that can
// only verify an object by reading it.
type getOnlyStore struct {
	inner *storagetest.MemoryStore
	gets  int
}

func (s *getOnlyStore) Put(ctx context.Context, key string, data []byte) error {
	return s.inner.Put(ctx, key, data)
}

func (s *getOnlyStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.inner.Get(ctx, key)
}

func (s *getOnlyStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	return s.inner.List(ctx, prefix)
}

func (s *getOnlyStore) Delete(ctx context.Context, key string) error {
	return s.inner.Delete(ctx, key)
}

// statCountingStore counts Gets on a store that can Stat.
type statCountingStore struct {
	*storagetest.MemoryStore
	gets int
}

func (s *statCountingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.MemoryStore.Get(ctx, key)
}

func TestPutMetadataForSourceChecksTheRecordedSource(t *testing.T) {
	source := []byte("compressed source bytes")
	sum := storage.SHA256Hex(source)
	cases := []struct {
		name    string
		stored  []byte // nil: missing
		sha     string
		size    int
		wantErr error
	}{
		{"matching", source, sum, len(source), nil},
		{"size unknown", source, sum, 0, nil},
		{"missing", nil, sum, len(source), storage.ErrNotFound},
		{"different bytes", []byte("other bytes"), sum, len(source), storage.ErrChecksumMismatch},
		{"same size, different bytes", []byte(strings.Repeat("x", len(source))), sum, len(source), storage.ErrChecksumMismatch},
		{"different size", source, sum, len(source) + 1, storage.ErrChecksumMismatch},
	}
	for _, statter := range []bool{true, false} {
		for _, test := range cases {
			t.Run(map[bool]string{true: "stat ", false: "get "}[statter]+test.name, func(t *testing.T) {
				memory := storagetest.NewMemoryStore()
				var store storage.ObjectStore = &statCountingStore{MemoryStore: memory}
				if !statter {
					store = &getOnlyStore{inner: memory}
				}
				if test.stored != nil {
					if err := memory.Put(context.Background(), "source", test.stored); err != nil {
						t.Fatal(err)
					}
				}
				err := storage.PutMetadataForSource(context.Background(), store, "source", test.sha, test.size, "metadata.json", []byte(`{}`), storage.RetryPolicy{MaxAttempts: 1})
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

func TestVerifyAccessWithoutObjectKeyerNotesRelativeKey(t *testing.T) {
	store := failingDeleteStore{storagetest.NewMemoryStore()}
	err := storage.VerifyAccess(context.Background(), store)
	if err == nil {
		t.Fatal("expected cleanup failure")
	}
	if !strings.Contains(err.Error(), `.setup-test/`) || !strings.Contains(err.Error(), "relative to the configured prefix") {
		t.Fatalf("cleanup error should name the relative key and note the prefix: %v", err)
	}
}

type failingDeleteStore struct{ *storagetest.MemoryStore }

func (failingDeleteStore) Delete(context.Context, string) error {
	return errors.New("delete denied")
}

func TestVerifyAccessUsesUniqueObjectAndCleansUp(t *testing.T) {
	store := storagetest.NewMemoryStore()
	if err := storage.VerifyAccess(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("setup object was not removed: %#v", objects)
	}
}

func TestSourceFirstPublicationReusesVerifiedSource(t *testing.T) {
	store := storagetest.NewMemoryStore()
	source := []byte(`{"schema_version":1}`)
	metadata := []byte(`{"source":"source.abc"}`)
	key := "sessions/codex/id/source." + storage.SHA256Hex(source) + ".jsonl.gz"
	if err := storage.PutSourceThenMetadata(context.Background(), store, key, "sessions/codex/id/metadata.json", source, metadata, storage.RetryPolicy{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "sessions/codex/id/metadata.json", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutSourceThenMetadata(context.Background(), store, key, "sessions/codex/id/metadata.json", source, metadata, storage.RetryPolicy{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "sessions/codex/id/metadata.json")
	if err != nil || string(got) != string(metadata) {
		t.Fatalf("metadata = %q, err = %v", got, err)
	}
}

type flakyStore struct {
	*storagetest.MemoryStore
	mu       sync.Mutex
	failPuts int
}

func (s *flakyStore) Put(ctx context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPuts > 0 {
		s.failPuts--
		return io.ErrUnexpectedEOF
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func TestSourcePublicationRetriesAndPublishesMetadataLast(t *testing.T) {
	store := &flakyStore{MemoryStore: storagetest.NewMemoryStore(), failPuts: 2}
	err := storage.PutSourceThenMetadata(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), storage.RetryPolicy{MaxAttempts: 3, InitialWait: time.Nanosecond, MaxWait: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil || len(objects) != 2 {
		t.Fatalf("objects = %#v, err = %v", objects, err)
	}
}

// Setup fails, naming the permissions page, when a missing object does not
// read as missing: the first publication of every session would fail the
// same way.
func TestVerifyAccessRequiresAMissingObjectToReadAsNotFound(t *testing.T) {
	for name, store := range map[string]storage.ObjectStore{
		"denied":        deniedAfterDeleteStore{storagetest.NewMemoryStore()},
		"still present": keepingDeleteStore{storagetest.NewMemoryStore()},
	} {
		err := storage.VerifyAccess(context.Background(), store)
		if err == nil {
			t.Fatalf("%s: storage.VerifyAccess succeeded", name)
		}
		if !strings.Contains(err.Error(), "read after delete") {
			t.Errorf("%s: error = %v, want the read-after-delete check named", name, err)
		}
	}
	err := storage.VerifyAccess(context.Background(), deniedAfterDeleteStore{storagetest.NewMemoryStore()})
	if !strings.Contains(err.Error(), "bucket-permissions.md") {
		t.Errorf("error = %v, want it to point at the permissions page", err)
	}
}

// deniedAfterDeleteStore answers a read of a deleted key with a permission
// error instead of storage.ErrNotFound.
type deniedAfterDeleteStore struct{ *storagetest.MemoryStore }

func (s deniedAfterDeleteStore) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := s.MemoryStore.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, responseError(http.StatusForbidden, errors.New("AccessDenied"))
	}
	return data, err
}

// keepingDeleteStore reports success from Delete without deleting.
type keepingDeleteStore struct{ *storagetest.MemoryStore }

func (keepingDeleteStore) Delete(context.Context, string) error { return nil }

type countingGetStore struct {
	*storagetest.MemoryStore
	gets int
}

func (s *countingGetStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.MemoryStore.Get(ctx, key)
}

// An existing source whose bytes differ will differ on every attempt.
func TestSourcePublicationDoesNotRetryChecksumMismatch(t *testing.T) {
	store := &countingGetStore{MemoryStore: storagetest.NewMemoryStore()}
	if err := store.Put(context.Background(), "source.hash", []byte("other bytes")); err != nil {
		t.Fatal(err)
	}
	err := storage.PutSourceThenMetadata(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), storage.RetryPolicy{MaxAttempts: 3, InitialWait: time.Nanosecond, MaxWait: time.Nanosecond})
	if !errors.Is(err, storage.ErrChecksumMismatch) || store.gets != 1 {
		t.Fatalf("gets = %d, err = %v", store.gets, err)
	}
}
