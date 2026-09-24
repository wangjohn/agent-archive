package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func (o Options) parserVersion() string {
	if o.ParserVersion != "" {
		return o.ParserVersion
	}
	return archive.DefaultParserVersion
}

// Metadata belongs in the publication state so a restart never confuses the
// parser used for a candidate with the parser actually published remotely.
func (s *LocalStore) loadPublishedMetadata(id string) ([]byte, error) {
	var state publishedState
	if err := local.Read(s.publishedPath(id), &state); err != nil {
		return nil, err
	}
	return state.MetadataBytes, nil
}

// Refresh from durable filtered evidence before opening the live transcript.
// A parser upgrade still works after the application rotates its local log.
//
// Regeneration is best effort and must never stand between a session and
// normal capture: whatever makes the retained publication unusable here (no
// cached metadata and no readable remote copy, or metadata that does not
// describe this machine's retained source) is a reason to skip, not to fail
// the session, because the next content change publishes current-parser
// metadata anyway. Only the publish attempt itself reports errors.
func regenerateMetadata(ctx context.Context, store *LocalStore, remote storage.ObjectStore, reg archive.SessionRegistration, now time.Time, opts Options) (sessionOutcome, bool, error) {
	// Only the real last publication is a valid source: a blocked, declined,
	// or rate-limited candidate cached alongside it was never made
	// discoverable, so a blocked session without one has nothing to refresh.
	bundle, _, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		return outcomeSkipped, false, err
	}
	encoded, err := store.loadPublishedMetadata(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, false, err
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, false, err
	}
	legacy := len(encoded) == 0
	if legacy {
		// One-time migration for publications made before metadata was cached.
		// A missing or unreachable copy is not fatal: nothing can be refreshed
		// from it, and normal capture keeps working without it.
		encoded, err = remote.Get(ctx, key)
		if err != nil {
			return outcomeSkipped, false, nil
		}
	}
	var prior archive.Metadata
	if err := json.Unmarshal(encoded, &prior); err != nil {
		return outcomeSkipped, false, nil
	}
	if prior.SessionID != reg.ArchiveSessionID || prior.MachineID != opts.MachineID || prior.ValidateSourceReference() != nil {
		return outcomeSkipped, false, nil
	}
	sameParser := prior.Parser.Version == opts.parserVersion()
	if sameParser && !legacy {
		return outcomeSkipped, false, nil
	}
	// The metadata must describe the source actually uploaded last, as
	// recorded at upload. (For state older than that record, lastPublishedSource
	// reads it from the cached metadata itself, so this holds trivially.)
	uploaded, known, err := store.LoadLastPublishedSource(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, false, err
	}
	if known && prior.SourceBundle != uploaded {
		return outcomeSkipped, false, nil
	}
	// A refresh this parser already found impossible for this publication is
	// not attempted again: each attempt costs a full decode and recompression
	// of the retained bundle, and possibly a download. A new publication
	// clears the record.
	if skipped, found, err := store.loadRefreshSkip(reg.ArchiveSessionID); err != nil {
		return outcomeSkipped, false, err
	} else if found && skipped.ParserVersion == opts.parserVersion() && skipped.SourceKey == prior.SourceBundle.Key {
		return outcomeSkipped, false, nil
	}
	// The source the refreshed metadata points at:
	//   - This build reproduces the uploaded source's exact bytes: they are
	//     carried, so the source is re-uploaded if it has gone missing.
	//   - It builds the retained bundle, but to other bytes (a compressor
	//     change): those bytes are published as a new source, which supersedes
	//     the old one. The remote is repaired whatever state the old object is
	//     in, and from then on the recorded source is one this build reproduces.
	//   - It cannot build the bundle at all (a source schema bump): only the
	//     recorded reference is carried, and the source is checked in storage
	//     against it (see publishPending).
	var source struct {
		ref   archive.SourceReference
		bytes []byte
	}
	if compressed, err := archive.BuildCompressedSource(bundle); err == nil {
		if key, err := archive.SourceObjectKey(bundle, compressed.SHA256); err == nil {
			source.ref = archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
			source.bytes = compressed.Bytes
		}
	}
	if source.bytes == nil {
		if !known {
			// Neither the bytes nor a recorded reference: nothing to publish
			// against. The next content change publishes current metadata.
			return outcomeSkipped, false, nil
		}
		source.ref = uploaded
	}
	// Cache before any early return below, so a legacy publication is
	// migrated exactly once rather than re-read on every scan.
	if err := store.cacheMetadata(reg.ArchiveSessionID, encoded); err != nil {
		return outcomeSkipped, false, err
	}
	if sameParser {
		// The same parser over the same retained source yields the same
		// result, including a failed parse: nothing to rebuild.
		return outcomeSkipped, false, nil
	}
	if liveTranscriptChanged(ctx, store, reg, bundle, now, opts) {
		// Normal capture is about to publish current-parser metadata with
		// the new content; a metadata-only publication first would only be
		// wasted work that also starts the upload interval early.
		return outcomeSkipped, false, nil
	}
	next, buildErr := archive.BuildMetadata(bundle, opts.MachineID, reg.SessionStartedAt, now, source.ref, archive.ParserInfo{Version: opts.parserVersion()})
	next.ApplyRegistrationProvenance(reg)
	if buildErr != nil && !archive.IsParseError(buildErr) {
		// This build cannot derive metadata from the retained bundle at all
		// (one cached under an older source schema, say). That is not a
		// failure of the session, but it is recorded, so it is not retried
		// on every pass and status can count it.
		return outcomeSkipped, false, store.saveRefreshSkip(reg.ArchiveSessionID, refreshSkip{ParserVersion: opts.parserVersion(), SourceKey: prior.SourceBundle.Key, Reason: refreshSkipUnderivable})
	}
	// Unchanged metadata over the same source needs no publication.
	comparison := next
	comparison.MetadataDerivedAt = prior.MetadataDerivedAt
	oldBytes, err := json.Marshal(prior)
	if err != nil {
		return outcomeSkipped, false, err
	}
	comparisonBytes, err := json.Marshal(comparison)
	if err != nil {
		return outcomeSkipped, false, err
	}
	if bytes.Equal(oldBytes, comparisonBytes) {
		return outcomeSkipped, false, nil
	}
	metadataBytes, err := json.Marshal(next)
	if err != nil {
		return outcomeSkipped, false, err
	}
	pending := PendingPublication{MetadataOnly: true, Bundle: bundle, SourceKey: source.ref.Key, MetadataKey: key, SourceSHA256: source.ref.SHA256, SourceBytes: source.bytes, SourceSize: source.ref.CompressedBytes, MetadataBytes: metadataBytes, ReadyAt: now}
	if err := store.SavePending(reg.ArchiveSessionID, pending); err != nil {
		return outcomeSkipped, false, err
	}
	outcome, err := publishPending(ctx, store, remote, reg.ArchiveSessionID, pending, now, opts)
	return outcome, true, err
}

// liveTranscriptChanged reports whether normal capture will publish this
// scan: the source (a transcript on disk, or a Cursor database chat) carries
// evidence the cached comparison bundle does not, and it still extends what
// capture guards against (the same rules processSession applies), so a
// rewrite that capture will only record as a gap does not count. A Cursor
// database chat that was rewritten does count: capture publishes it. It compares against the cached candidate rather
// than only the last publication so a declined candidate the transcript
// still matches leaves regeneration free to proceed. A transcript that
// cannot be compared (rotated, oversize, unsafe) reports no change:
// regeneration is then the only way the summary can move.
func liveTranscriptChanged(ctx context.Context, store *LocalStore, reg archive.SessionRegistration, lastPublished archive.SourceBundle, now time.Time, opts Options) bool {
	source, ok := newSourceReader(reg, opts)
	if !ok {
		return false
	}
	cached, _, status, found, err := store.LoadPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		return false
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return false
	}
	filtered, _, err := source.Filter(ctx, adapter, opts.maxTranscriptBytes())
	if err != nil {
		return false
	}
	candidate, err := archive.NewSourceBundle(reg, adapter, filtered, now, cached.SupplementalEvidence)
	if err != nil {
		return false
	}
	same, err := bundleEvidenceEqual(cached, candidate)
	if err != nil || same {
		return false
	}
	if reg.SourceKind == archive.SourceKindCursorSQLite {
		return true
	}
	guard := cached
	if status == CacheStatusBlocked {
		guard = lastPublished
	}
	return nativeEvidenceExtends(guard, candidate)
}

// refreshSkip records that the current parser cannot refresh a session's
// last publication's metadata, so regenerateMetadata stops retrying it. It
// names no content: only the parser version, the published source's object
// key, and why.
type refreshSkip struct {
	ParserVersion string            `json:"parser_version"`
	SourceKey     string            `json:"source_key"`
	Reason        refreshSkipReason `json:"reason"`
}

type refreshSkipReason string

const (
	// refreshSkipUnderivable: this build cannot derive metadata from the
	// retained bundle at all.
	refreshSkipUnderivable refreshSkipReason = "metadata_underivable"
	// refreshSkipSourceUnavailable: the recorded source is missing from
	// storage, or differs from its record, and this build cannot rebuild it.
	refreshSkipSourceUnavailable refreshSkipReason = "source_unavailable"
)

const refreshSkipDir = "refresh-skips"

func (s *LocalStore) refreshSkipPath(id string) string {
	return filepath.Join(s.home, refreshSkipDir, id+".json")
}

func (s *LocalStore) saveRefreshSkip(id string, skip refreshSkip) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	return local.Write(s.refreshSkipPath(id), skip)
}

// loadRefreshSkip returns a session's refresh-skip record. One that cannot
// be read is treated as absent: the refresh is then simply tried again.
func (s *LocalStore) loadRefreshSkip(id string) (refreshSkip, bool, error) {
	if !safeFileComponent(id) {
		return refreshSkip{}, false, errors.New("archive session ID is not a safe file name component")
	}
	var skip refreshSkip
	if err := local.Read(s.refreshSkipPath(id), &skip); err != nil {
		return refreshSkip{}, false, nil
	}
	return skip, true, nil
}

// removeRefreshSkip drops a session's record once a publication makes it
// stale.
func (s *LocalStore) removeRefreshSkip(id string) error {
	err := os.Remove(s.refreshSkipPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove refresh-skip record %q: %w", id, err)
	}
	return nil
}

// countRefreshSkips counts the sessions whose metadata parserVersion cannot
// refresh (see Status.UnrefreshableSummaries).
func (s *LocalStore) countRefreshSkips(parserVersion string) int {
	entries, err := os.ReadDir(filepath.Join(s.home, refreshSkipDir))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var skip refreshSkip
		if local.Read(filepath.Join(s.home, refreshSkipDir, entry.Name()), &skip) == nil && skip.ParserVersion == parserVersion {
			count++
		}
	}
	return count
}

func (s *LocalStore) cacheMetadata(id string, metadata []byte) error {
	var state publishedState
	if err := local.Read(s.publishedPath(id), &state); err != nil {
		return err
	}
	if bytes.Equal(state.MetadataBytes, metadata) {
		return nil
	}
	state.MetadataBytes = metadata
	return local.Write(s.publishedPath(id), state)
}

// Updating a summary must not discard a richer local-only candidate.
func (s *LocalStore) saveRepublishedMetadata(id string, pending PendingPublication, at time.Time) error {
	var state publishedState
	if err := local.Read(s.publishedPath(id), &state); err != nil {
		return err
	}
	state.MetadataBytes = pending.MetadataBytes
	state.PublishedAt = at
	source := pending.sourceReference()
	if state.Status == CacheStatusPublished {
		// The current bundle is the republished one, so the snapshot shares it.
		state.Bundle = pending.Bundle
		state.LastPublished = &publishedSnapshot{PublishedAt: at, SameAsBundle: true, Source: &source}
	} else {
		state.LastPublished = &publishedSnapshot{Bundle: pending.Bundle, PublishedAt: at, Source: &source}
	}
	return local.Write(s.publishedPath(id), state)
}
