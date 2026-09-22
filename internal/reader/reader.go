// Package reader lists private metadata and reads selected source bundles into
// memory. It creates no normalized persistence. The only thing it may keep on
// disk is the disposable metadata cache (see MetadataCache); source bundles
// are never written locally.
package reader

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

var ErrRefreshRequired = errors.New("source changed or was deleted; refresh metadata and retry")

// SkillUsage narrows a Filter's Skill/SkillSHA256 match to a specific
// relationship between a session and the named skill. The zero value means
// "the skill was used".
type SkillUsage string

const (
	// SkillUsageUsed is the default (zero-value) behavior: match sessions
	// that actually used the skill.
	SkillUsageUsed SkillUsage = "used"
	// SkillUsageAvailable matches sessions where the skill was available
	// (eligible or discovered coverage), regardless of whether it was used.
	SkillUsageAvailable SkillUsage = "available"
	// SkillUsageEligibleNoUse matches sessions where the skill was eligible
	// but never used. It requires the metadata's SkillDetection to be
	// archive.SkillDetectionObservedNone; anything else (including
	// archive.SkillDetectionUnavailable) is treated as unknown, not "no use".
	//
	// No parser version emits archive.SkillDetectionObservedNone today, so
	// this value cannot match any sidecar yet. It stays accepted, and the
	// comparison stays implemented, for the parser version that will.
	SkillUsageEligibleNoUse SkillUsage = "eligible_no_use"
)

type Filter struct {
	Harness, Model, Skill, SkillSHA256 string
	From, To                           time.Time
	RequireCompleteCoverage            bool
	SkillUsage                         SkillUsage
}
type Limits struct{ MaxCompressedBytes, MaxUncompressedBytes int }

func (l Limits) compressed() int {
	if l.MaxCompressedBytes > 0 {
		return l.MaxCompressedBytes
	}
	return 32 << 20
}
func (l Limits) uncompressed() int {
	if l.MaxUncompressedBytes > 0 {
		return l.MaxUncompressedBytes
	}
	return 128 << 20
}

// Harnesses are the harness segments this build publishes metadata under:
// every sidecar key is "sessions/<harness>/<id>/metadata.json".
var Harnesses = []string{"claude", "codex", "cursor"}

// listConcurrency bounds how many sidecars a listing downloads at once. The
// sidecars are small and independent, so a sequential read spends almost all
// of its time waiting on round trips; eight keeps a slow link busy without
// flooding a provider.
const listConcurrency = 8

// ListOptions tunes ListMetadataWithOptions. The zero value reads every
// matching sidecar from the store.
type ListOptions struct {
	// Cache, when set, serves a sidecar whose listed ETag is unchanged from
	// local disk instead of downloading it, and forgets sidecars which are
	// no longer listed. It holds metadata only.
	Cache *MetadataCache
}

// ListMetadata reads only metadata sidecars and applies filters without
// downloading transcript bundles.
func ListMetadata(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter) ([]archive.Metadata, error) {
	return ListMetadataWithOptions(ctx, store, prefix, filter, ListOptions{})
}

// ListMetadataWithOptions is ListMetadata with an optional local cache. A
// harness filter narrows the listing to that harness's own prefix, keys which
// are not sidecars are skipped before any download, and sidecars are read with
// bounded concurrency. Results are ordered newest capture first, and the first
// sidecar in key order which cannot be read or validated fails the listing, as
// a sequential read would.
func ListMetadataWithOptions(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, options ListOptions) ([]archive.Metadata, error) {
	listPrefix := listPrefixFor(prefix, filter.Harness)
	objects, err := store.List(ctx, listPrefix)
	if err != nil {
		return nil, err
	}
	sidecars := make([]storage.Object, 0, len(objects))
	for _, object := range objects {
		if strings.HasSuffix(object.Key, "/metadata.json") {
			sidecars = append(sidecars, object)
		}
	}
	loaded, err := readSidecars(ctx, store, sidecars, options.Cache)
	if err != nil {
		return nil, err
	}
	if options.Cache != nil {
		options.Cache.evictUnlisted(listPrefix, sidecars)
	}
	var results []archive.Metadata
	for _, metadata := range loaded {
		if matches(metadata, filter) {
			results = append(results, metadata)
		}
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].CapturedAt.After(results[j].CapturedAt) })
	return results, nil
}

// listPrefixFor narrows a listing to one harness's sessions. A harness value
// that cannot be a key segment lists the whole prefix instead, which is what
// every harness filter did before, so it still matches nothing rather than
// becoming an error.
func listPrefixFor(prefix, harness string) string {
	if harness == "" {
		return prefix
	}
	if _, err := archive.MetadataObjectKey(harness, "probe"); err != nil {
		return prefix
	}
	if trimmed := strings.TrimSuffix(prefix, "/"); trimmed != "" {
		return trimmed + "/" + harness + "/"
	}
	return harness + "/"
}

// readSidecars downloads and validates each listed sidecar with at most
// listConcurrency requests in flight. Work is dispatched in key order and no
// new work starts after a failure or once ctx is done, so every sidecar before
// the first failing one has been read and the lowest-index error is the one a
// sequential read would have returned. It returns only after every read it
// started has finished, so no goroutine outlives the call, and a cancelled
// ctx is reported even when the store itself ignores it.
func readSidecars(ctx context.Context, store storage.ObjectStore, objects []storage.Object, cache *MetadataCache) ([]archive.Metadata, error) {
	out := make([]archive.Metadata, len(objects))
	errs := make([]error, len(objects))
	var failed atomic.Bool
	slots := make(chan struct{}, listConcurrency)
	var wg sync.WaitGroup
	dispatched := 0
	for index, object := range objects {
		if failed.Load() || ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		// A failure may have landed while waiting for the slot.
		if failed.Load() || ctx.Err() != nil {
			<-slots
			break
		}
		dispatched++
		wg.Add(1)
		go func(index int, object storage.Object) {
			defer wg.Done()
			defer func() { <-slots }()
			metadata, err := readListedSidecar(ctx, store, object, cache)
			if err != nil {
				errs[index] = err
				failed.Store(true)
				return
			}
			out[index] = metadata
		}(index, object)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if dispatched < len(objects) {
		// Dispatch stopped early without a read failing: ctx is done.
		return nil, fmt.Errorf("read metadata: %w", ctx.Err())
	}
	return out, nil
}

// readListedSidecar serves an unchanged sidecar from the cache and otherwise
// downloads it. Only a sidecar which decoded and validated is cached, and a
// cache entry which no longer decodes is treated as a miss.
func readListedSidecar(ctx context.Context, store storage.ObjectStore, object storage.Object, cache *MetadataCache) (archive.Metadata, error) {
	if data, ok := cache.get(object.Key, object.ETag); ok {
		if metadata, err := decodeMetadata(object.Key, data); err == nil {
			return metadata, nil
		}
	}
	data, err := store.Get(ctx, object.Key)
	if err != nil {
		return archive.Metadata{}, fmt.Errorf("read metadata %q: %w", object.Key, err)
	}
	metadata, err := decodeMetadata(object.Key, data)
	if err != nil {
		return archive.Metadata{}, err
	}
	cache.put(object.Key, object.ETag, data)
	return metadata, nil
}

// ReadMetadata reads and validates one metadata sidecar by its object key.
// Like ListMetadata it downloads no transcript bundle.
func ReadMetadata(ctx context.Context, store storage.ObjectStore, key string) (archive.Metadata, error) {
	data, err := store.Get(ctx, key)
	if err != nil {
		return archive.Metadata{}, fmt.Errorf("read metadata %q: %w", key, err)
	}
	return decodeMetadata(key, data)
}

func decodeMetadata(key string, data []byte) (archive.Metadata, error) {
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return archive.Metadata{}, fmt.Errorf("decode metadata %q: %w", key, err)
	}
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.Metadata{}, fmt.Errorf("invalid metadata %q: %w", key, err)
	}
	return metadata, nil
}

// FindMetadataKeys returns the metadata sidecar keys under prefix that
// belong to one archive session ID, in key order. A caller that only has the
// ID does not know its harness segment, so the sidecar key is tried under
// each harness this build publishes (Harnesses) with a direct read, and only
// if none exists is the prefix listed — a listing is proportional to the whole
// archive, three reads are not. It returns keys only; the probe reads are not
// kept. More than one result means the same ID was published under more than
// one harness, which a caller should treat as ambiguous. A read error other
// than not-found is returned rather than falling back.
func FindMetadataKeys(ctx context.Context, store storage.ObjectStore, prefix, archiveSessionID string) ([]string, error) {
	if archiveSessionID == "" || strings.Contains(archiveSessionID, "/") {
		return nil, fmt.Errorf("invalid archive session ID %q", archiveSessionID)
	}
	if _, err := archive.MetadataObjectKey(Harnesses[0], archiveSessionID); err != nil {
		return nil, fmt.Errorf("invalid archive session ID %q", archiveSessionID)
	}
	var keys []string
	for _, harness := range Harnesses {
		key := strings.TrimPrefix(listPrefixFor(prefix, harness)+archiveSessionID+"/metadata.json", "/")
		if _, err := store.Get(ctx, key); err == nil {
			keys = append(keys, key)
		} else if !errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("read metadata %q: %w", key, err)
		}
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		return keys, nil
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	suffix := "/" + archiveSessionID + "/metadata.json"
	for _, object := range objects {
		if strings.HasSuffix(object.Key, suffix) {
			keys = append(keys, object.Key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// eligibilityParserVersion is the lowest parser version whose observed_none
// could rest on complete eligibility and use-observation coverage rather than
// on inference from availability alone.
var eligibilityParserVersion = [3]int{0, 4, 0}

// parserVersionAtLeast reports whether version parses as a plain numeric
// major.minor.patch triple at or above min. Anything unparsable — the empty
// string, a custom collector.Options.ParserVersion override, a pre-release
// suffix — is not at least min, because only a version this function can
// order says which parser wrote the sidecar.
func parserVersionAtLeast(version string, min [3]int) bool {
	fields := strings.Split(version, ".")
	if len(fields) != len(min) {
		return false
	}
	var parsed [3]int
	for i, field := range fields {
		if field == "" || len(field) > 1 && field[0] == '0' {
			return false
		}
		for _, r := range field {
			if r < '0' || r > '9' {
				return false
			}
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return false
		}
		parsed[i] = n
	}
	for i := range parsed {
		if parsed[i] != min[i] {
			return parsed[i] > min[i]
		}
	}
	return true
}

func matches(m archive.Metadata, f Filter) bool {
	if f.Harness != "" && m.Harness.Name != f.Harness {
		return false
	}
	if !f.From.IsZero() && m.CapturedAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && m.CapturedAt.After(f.To) {
		return false
	}
	if f.RequireCompleteCoverage && (m.Parser.Status != archive.ParserStatusComplete || len(m.CaptureGaps) != 0) {
		return false
	}
	if f.Model != "" {
		found := false
		for _, x := range m.Models {
			if x.Attributes["gen_ai.request.model"] == f.Model || x.Attributes["gen_ai.response.model"] == f.Model {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if f.Skill != "" || f.SkillSHA256 != "" {
		used, available := false, false
		for _, x := range m.SkillsUsed {
			if (f.Skill == "" || x.Name == f.Skill) && (f.SkillSHA256 == "" || x.SHA256 == f.SkillSHA256) {
				used = true
			}
		}
		for _, x := range m.SkillsAvailable {
			if (f.Skill == "" || x.Name == f.Skill) && (f.SkillSHA256 == "" || x.SHA256 == f.SkillSHA256) {
				if x.Coverage == archive.SkillCoverageEligible || x.Coverage == archive.SkillCoverageDiscovered {
					available = true
				}
			}
		}
		// Older parsers inferred non-use from availability alone. Those
		// sidecars remain readable, but cannot support a no-use comparison.
		// This is a whitelist, not a blacklist of known-legacy strings:
		// collector.Options.ParserVersion is a real override, so a pre-0.4.0
		// build could have written observed_none under any version string.
		trustedDetection := parserVersionAtLeast(m.Parser.Version, eligibilityParserVersion)
		eligibleNoUse := available && !used && trustedDetection && m.SkillDetection == archive.SkillDetectionObservedNone
		switch f.SkillUsage {
		case SkillUsageAvailable:
			if !available {
				return false
			}
		case SkillUsageEligibleNoUse:
			if !eligibleNoUse {
				return false
			}
		default: // SkillUsageUsed, or the zero value
			if !used {
				return false
			}
		}
	}
	return true
}

// LoadSource verifies the compressed SHA-256 before bounded decompression and
// validates that source identity matches the selected metadata pointer.
func LoadSource(ctx context.Context, store storage.ObjectStore, metadata archive.Metadata, limits Limits) (archive.SourceBundle, error) {
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.SourceBundle{}, err
	}
	data, err := store.Get(ctx, metadata.SourceBundle.Key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return archive.SourceBundle{}, ErrRefreshRequired
		}
		return archive.SourceBundle{}, err
	}
	if len(data) > limits.compressed() {
		return archive.SourceBundle{}, errors.New("source exceeds compressed read limit")
	}
	if len(data) != metadata.SourceBundle.CompressedBytes {
		return archive.SourceBundle{}, errors.New("source compressed size does not match metadata")
	}
	if !storage.VerifySHA256(data, metadata.SourceBundle.SHA256) {
		return archive.SourceBundle{}, errors.New("source checksum mismatch")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return archive.SourceBundle{}, fmt.Errorf("open source gzip: %w", err)
	}
	defer gz.Close()
	plain, err := io.ReadAll(io.LimitReader(gz, int64(limits.uncompressed()+1)))
	if err != nil {
		return archive.SourceBundle{}, err
	}
	if len(plain) > limits.uncompressed() {
		return archive.SourceBundle{}, errors.New("source exceeds uncompressed read limit")
	}
	var bundle archive.SourceBundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		return archive.SourceBundle{}, fmt.Errorf("decode source: %w", err)
	}
	if bundle.SchemaVersion != archive.SourceSchemaVersion || bundle.ArchiveSessionID != metadata.SessionID || bundle.NativeSessionID != metadata.NativeSessionID || bundle.ProjectID != metadata.ProjectID || bundle.ParentSessionID != metadata.ParentSessionID || bundle.Capture.Harness != metadata.Harness || !bundle.Capture.CapturedAt.Equal(metadata.CapturedAt) || bundle.Capture.FilterVersion != metadata.FilterVersion {
		return archive.SourceBundle{}, errors.New("source identity does not match metadata")
	}
	key, err := archive.SourceObjectKey(bundle, metadata.SourceBundle.SHA256)
	if err != nil || key != metadata.SourceBundle.Key {
		return archive.SourceBundle{}, errors.New("source key does not match metadata identity")
	}
	return bundle, nil
}

// RefreshAndLoad retries once after rereading metadata, covering the normal
// metadata-pointer refresh race after old source cleanup.
func RefreshAndLoad(ctx context.Context, store storage.ObjectStore, metadataKey string, limits Limits) (archive.Metadata, archive.SourceBundle, error) {
	for attempt := 0; attempt < 2; attempt++ {
		m, err := ReadMetadata(ctx, store, metadataKey)
		if err != nil {
			return archive.Metadata{}, archive.SourceBundle{}, err
		}
		b, err := LoadSource(ctx, store, m, limits)
		if !errors.Is(err, ErrRefreshRequired) || attempt == 1 {
			return m, b, err
		}
	}
	return archive.Metadata{}, archive.SourceBundle{}, ErrRefreshRequired
}
