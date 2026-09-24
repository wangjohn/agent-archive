package reader

import (
	"crypto/md5"
	"crypto/sha256"
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
// directories 0700 and every file 0600. Files are written atomically (temp
// file, then rename) but without fsync: the cache is rebuilt from the store
// on any miss, so a copy lost to a crash costs one download, and each entry
// carries a SHA-256 of its bytes so a torn or damaged file is a miss rather
// than a wrong answer.
//
// Staleness: the bytes come from a Get that runs after the listing, so an
// object rewritten in between would be stored under the old ETag. S3, R2 and
// MinIO report the MD5 of a single-part object as its ETag (the collector
// publishes sidecars with a single PutObject), so when the listed ETag is a
// bare MD5 the bytes are only cached if they hash to it, which rules the race
// out. An ETag in another form (a multipart "-N" suffix, SSE-KMS) cannot be
// checked; such an entry could be wrong only if the object was rewritten
// between the listing and the Get and then rewritten back to the listed
// bytes before the next listing, at which point the next listing's ETag would
// again match the entry. That window is accepted and `list --no-cache` is
// always available.
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
	SHA256   string          `json:"sha256"`
	Metadata json.RawMessage `json:"metadata"`
}

func isMetadataKey(key string) bool { return strings.HasSuffix(key, "/metadata.json") }

func (c *MetadataCache) path(key string) (string, bool) {
	if c == nil || !isMetadataKey(key) || len(key) > maxCacheKeyBytes {
		return "", false
	}
	return filepath.Join(c.dir, hex.EncodeToString([]byte(key))+".json"), true
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// etagMatchesBytes reports whether the listed ETag can be checked against the
// downloaded bytes and, if so, whether it matches. A bare 32-hex ETag is the
// MD5 of a single-part object on S3-compatible stores; any other form (quoted
// values are already trimmed by the store) is unverifiable and returns ok
// without a match.
func etagMatchesBytes(etag string, data []byte) (verifiable, matches bool) {
	if len(etag) != 32 {
		return false, false
	}
	if _, err := hex.DecodeString(etag); err != nil {
		return false, false
	}
	sum := md5.Sum(data) //nolint:gosec // S3 ETag comparison, not a security hash
	return true, strings.EqualFold(hex.EncodeToString(sum[:]), etag)
}

// get returns the cached sidecar bytes for key when they were cached under
// exactly this ETag and still hash to the digest recorded with them. An empty
// ETag never matches: without one nothing proves the cached copy is current.
func (c *MetadataCache) get(key, etag string) ([]byte, bool) {
	path, ok := c.path(key)
	if !ok || etag == "" {
		return nil, false
	}
	var entry metadataCacheEntry
	if err := local.Read(path, &entry); err != nil {
		return nil, false
	}
	if entry.Key != key || entry.ETag != etag || len(entry.Metadata) == 0 || entry.SHA256 != sha256Hex(entry.Metadata) {
		return nil, false
	}
	return entry.Metadata, true
}

// put caches one validated sidecar under the ETag the listing reported. When
// that ETag is a checkable content hash, bytes which do not hash to it were
// rewritten after the listing and are not cached. It is best effort: a cache
// that cannot be written only costs a download next time.
func (c *MetadataCache) put(key, etag string, data []byte) {
	path, ok := c.path(key)
	if !ok || etag == "" || !json.Valid(data) {
		return
	}
	if verifiable, matches := etagMatchesBytes(etag, data); verifiable && !matches {
		return
	}
	encoded, err := json.Marshal(metadataCacheEntry{Key: key, ETag: etag, SHA256: sha256Hex(data), Metadata: data})
	if err != nil {
		return
	}
	_ = writeCacheFile(path, encoded)
}

// writeCacheFile replaces path atomically with a 0600 file. Unlike
// local.WriteBytes it does not fsync: this cache is disposable and rebuilt on
// any miss, and the per-file fsync was the whole cost of a cold `list`.
func writeCacheFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-")
	if err != nil {
		return err
	}
	// After a successful rename there is nothing left to remove.
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
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
