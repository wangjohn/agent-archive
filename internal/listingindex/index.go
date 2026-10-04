// Package listingindex stores immutable, time-ordered hints for bounded
// browsing. The mutable metadata sidecar remains the sole authority.
package listingindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Prefix is the key prefix for immutable time-ordered listing hints.
const Prefix = "listing/v1/"

const bySessionPrefix = "listing/by-session/"

const maxTime = uint64(9999999999999999999)

// Entry identifies the exact sidecar revision for one index hint. Hash is
// compared with freshly downloaded bytes, so old hints cannot revive an
// overwritten sidecar.
type Entry struct {
	Key         string
	MetadataKey string
	Hash        string
	CapturedAt  time.Time
}

// New constructs a hint for one validated metadata sidecar.
func New(metadataKey string, data []byte) (Entry, error) {
	var m archive.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Entry{}, err
	}
	if err := m.ValidateSourceReference(); err != nil {
		return Entry{}, err
	}
	expected, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil || expected != metadataKey {
		return Entry{}, fmt.Errorf("metadata key does not match session")
	}
	if m.CapturedAt.IsZero() || m.CapturedAt.UnixNano() < 0 {
		return Entry{}, fmt.Errorf("invalid capture time")
	}
	hash := storage.SHA256Hex(data)
	reverse := maxTime - uint64(m.CapturedAt.UnixNano())
	return Entry{Key: fmt.Sprintf("%s%019d/%s/%s/%s.json", Prefix, reverse, m.Harness.Name, m.SessionID, hash), MetadataKey: metadataKey, Hash: hash, CapturedAt: m.CapturedAt}, nil
}

// Parse decodes a listing hint key and rejects malformed components.
func Parse(key string) (Entry, error) {
	if !strings.HasPrefix(key, Prefix) {
		return Entry{}, errors.New("not a listing index key")
	}
	parts := strings.Split(strings.TrimPrefix(key, Prefix), "/")
	if len(parts) != 4 || !strings.HasSuffix(parts[3], ".json") {
		return Entry{}, errors.New("invalid listing index key")
	}
	reverse, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || reverse > maxTime {
		return Entry{}, errors.New("invalid listing timestamp")
	}
	if maxTime-reverse > uint64(1<<63-1) {
		return Entry{}, errors.New("listing timestamp out of range")
	}
	metadataKey, err := archive.MetadataObjectKey(parts[1], parts[2])
	if err != nil {
		return Entry{}, err
	}
	hash := strings.TrimSuffix(parts[3], ".json")
	if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
		return Entry{}, errors.New("invalid listing hash")
	}
	return Entry{Key: key, MetadataKey: metadataKey, Hash: hash, CapturedAt: time.Unix(0, int64(maxTime-reverse)).UTC()}, nil //nolint:gosec // subtraction is checked against MaxInt64 above
}

// SessionPrefix returns the pointer-key prefix for one archive session.
func SessionPrefix(harness, id string) (string, error) {
	if _, err := archive.MetadataObjectKey(harness, id); err != nil {
		return "", err
	}
	return bySessionPrefix + harness + "/" + id + "/", nil
}

// DeleteSession removes index hints after authoritative metadata deletion.
// Leftover hints after an interruption are safe because readers verify the
// current sidecar, and the next cleanup can retry.
func DeleteSession(ctx context.Context, store storage.ObjectStore, harness, id string) error {
	prefix, err := SessionPrefix(harness, id)
	if err != nil {
		return err
	}
	metadataKey, err := archive.MetadataObjectKey(harness, id)
	if err != nil {
		return err
	}
	pointers, err := store.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, pointer := range pointers {
		key, err := store.Get(ctx, pointer.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		entry, parseErr := Parse(string(key))
		if parseErr == nil && entry.MetadataKey == metadataKey && entry.Hash == path.Base(pointer.Key) {
			if err := store.Delete(ctx, entry.Key); err != nil {
				return err
			}
		}
		if err := store.Delete(ctx, pointer.Key); err != nil {
			return err
		}
	}
	return deleteRevisions(ctx, store, harness, id)
}
