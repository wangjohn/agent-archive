package reader

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// MetadataCache is a disposable local copy of metadata sidecars, keyed by
// object key and the ETag the store listed for it. A listing still happens
// every time, so the cache never decides what exists: it only saves the
// download of a sidecar whose ETag has not changed, and it forgets any sidecar
// the listing no longer returns, which keeps it inside the archive's retention
// policy. It holds metadata sidecars only — put refuses any other key — and is
// never authoritative: every failure to read or write it is a miss.
//
// Layout: AGENT_ARCHIVE_HOME/cache/metadata/<hex(object key)>.json, with both
// directories 0700 and every file 0600 (written by local.WriteBytes).
type MetadataCache struct {
	dir string
}

// maxCacheKeyBytes keeps hex(key) comfortably inside a file name limit. A
// longer key is simply not cached.
const maxCacheKeyBytes = 120

// OpenMetadataCache creates (or reuses) the metadata cache under home.
func OpenMetadataCache(home string) (*MetadataCache, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("metadata cache requires a data directory")
	}
	root := filepath.Join(home, "cache")
	dir := filepath.Join(root, "metadata")
	for _, path := range []string{root, dir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, err
		}
	}
	return &MetadataCache{dir: dir}, nil
}

type metadataCacheEntry struct {
	Key      string          `json:"key"`
	ETag     string          `json:"etag"`
	Metadata json.RawMessage `json:"metadata"`
}

func isMetadataKey(key string) bool { return strings.HasSuffix(key, "/metadata.json") }

func (c *MetadataCache) path(key string) (string, bool) {
	if c == nil || !isMetadataKey(key) || len(key) > maxCacheKeyBytes {
		return "", false
	}
	return filepath.Join(c.dir, hex.EncodeToString([]byte(key))+".json"), true
}

// get returns the cached sidecar bytes for key when they were cached under
// exactly this ETag. An empty ETag never matches: without one nothing proves
// the cached copy is current.
func (c *MetadataCache) get(key, etag string) ([]byte, bool) {
	path, ok := c.path(key)
	if !ok || etag == "" {
		return nil, false
	}
	var entry metadataCacheEntry
	if err := local.Read(path, &entry); err != nil {
		return nil, false
	}
	if entry.Key != key || entry.ETag != etag || len(entry.Metadata) == 0 {
		return nil, false
	}
	return entry.Metadata, true
}

// put caches one validated sidecar. It is best effort: a cache that cannot be
// written only costs a download next time.
func (c *MetadataCache) put(key, etag string, data []byte) {
	path, ok := c.path(key)
	if !ok || etag == "" || !json.Valid(data) {
		return
	}
	encoded, err := json.Marshal(metadataCacheEntry{Key: key, ETag: etag, Metadata: data})
	if err != nil {
		return
	}
	_ = local.WriteBytes(path, encoded)
}

// evictUnlisted removes every cached sidecar under listPrefix which the
// listing did not return. Entries outside listPrefix belong to a listing this
// one did not cover (another harness) and are left alone.
func (c *MetadataCache) evictUnlisted(listPrefix string, listed []storage.Object) {
	if c == nil {
		return
	}
	present := make(map[string]bool, len(listed))
	for _, object := range listed {
		present[object.Key] = true
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := hex.DecodeString(strings.TrimSuffix(name, ".json"))
		if err != nil {
			// Not a name this cache writes; it cannot be one of ours to keep.
			_ = os.Remove(filepath.Join(c.dir, name))
			continue
		}
		key := string(raw)
		if strings.HasPrefix(key, listPrefix) && !present[key] {
			_ = os.Remove(filepath.Join(c.dir, name))
		}
	}
}
