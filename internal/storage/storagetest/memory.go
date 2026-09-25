// Package storagetest is an in-memory storage.ObjectStore for tests. It is
// test code only: production code stores through storage's S3 client, and
// depguard (.golangci.yml) keeps this package out of it.
package storagetest

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// MemoryStore is a concurrency-safe in-memory storage.ObjectStore for tests. It intentionally returns copies so callers cannot mutate stored
// bytes after a successful Put.
type MemoryStore struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
}

type memoryObject struct {
	data []byte
	etag string
	when time.Time
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: make(map[string]memoryObject)}
}

// Put stores a copy of data under key, replacing any existing object. The
// object's ETag is the MD5 of its bytes, as S3 reports for a single-part
// upload.
func (s *MemoryStore) Put(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	copyData := append([]byte(nil), data...)
	// The ETag is the MD5 of the bytes, exactly what S3, R2 and MinIO report
	// for a single-part upload: it changes when the bytes change, and a
	// reader can check downloaded bytes against it, which the metadata cache
	// relies on.
	s.objects[key] = memoryObject{data: copyData, etag: md5Hex(copyData), when: time.Now().UTC()}
	return nil
}

// Get returns a copy of the object's bytes, or storage.ErrNotFound.
func (s *MemoryStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return append([]byte(nil), obj.data...), nil
}

// Stat describes an object, with the SHA-256 of its bytes.
func (s *MemoryStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	obj, ok := s.objects[key]
	if !ok {
		return storage.ObjectInfo{}, storage.ErrNotFound
	}
	return storage.ObjectInfo{Size: int64(len(obj.data)), SHA256: storage.SHA256Hex(obj.data)}, nil
}

// List returns the objects under prefix, sorted by key, without their
// bodies. A prefix matches whole path components: "a" lists "a" and "a/b"
// but not "ab". An empty prefix lists everything.
func (s *MemoryStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if len(prefix) == 0 || hasPrefixKey(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	objects := make([]storage.Object, 0, len(keys))
	for _, key := range keys {
		obj := s.objects[key]
		objects = append(objects, storage.Object{Key: key, Size: int64(len(obj.data)), ETag: obj.etag, LastModified: obj.when})
	}
	return objects, nil
}

// Delete removes the object at key; a missing object is not an error.
func (s *MemoryStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func hasPrefixKey(key, prefix string) bool {
	if len(key) < len(prefix) || key[:len(prefix)] != prefix {
		return false
	}
	return len(key) == len(prefix) || prefix[len(prefix)-1] == '/' || key[len(prefix)] == '/'
}

var (
	_ storage.ObjectStore   = (*MemoryStore)(nil)
	_ storage.ObjectStatter = (*MemoryStore)(nil)
)

// md5Hex is the ETag an S3-compatible store reports for a single-part,
// non-KMS object. It is an identity for ETag comparison, not a security hash.
func md5Hex(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec
	return hex.EncodeToString(sum[:])
}
