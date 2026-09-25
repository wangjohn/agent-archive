package collector

import (
	"bytes"
	"encoding/json"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

func (o Options) parserVersion() string {
	if o.ParserVersion != "" {
		return o.ParserVersion
	}
	return archive.DefaultParserVersion
}

// regenerateMetadata is the scan's refresh step: it re-derives the last
// publication's metadata from durable filtered evidence before opening the
// live transcript, so a parser upgrade still works after the application
// rotates its local log. handled reports that the scan ends here.
//
// Regeneration is best effort and must never stand between a session and
// normal capture: whatever makes the retained publication unusable here (no
// cached metadata and no readable remote copy, or metadata that does not
// describe this machine's retained source) is a reason to skip, not to fail
// the session, because the next content change publishes current-parser
// metadata anyway. Only the publish attempt itself reports errors.
func regenerateMetadata(s *sessionScan) (outcome sessionOutcome, handled bool, err error) {
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return outcomeSkipped, false, err
	}
	last, ok := s.lastPublication(key)
	if !ok {
		return outcomeSkipped, false, nil
	}
	prior := last.metadata
	sameParser := prior.Parser.Version == s.opts.parserVersion()
	if sameParser && !last.legacy {
		return outcomeSkipped, false, nil
	}
	// The metadata must describe the source actually uploaded last, as
	// recorded at upload. (For state older than that record,
	// LastPublishedSource reads it from the cached metadata itself, so this
	// holds trivially.)
	uploaded, known := s.published.LastPublishedSource()
	if known && prior.SourceBundle != uploaded {
		return outcomeSkipped, false, nil
	}
	// A refresh this parser already found impossible for this publication is
	// not attempted again: each attempt costs a full decode and recompression
	// of the retained bundle, and possibly a download. A new publication
	// clears the record.
	if skipped, found, err := s.local.LoadRefreshSkip(s.id()); err != nil {
		return outcomeSkipped, false, err
	} else if found && skipped.ParserVersion == s.opts.parserVersion() && skipped.SourceKey == prior.SourceBundle.Key {
		return outcomeSkipped, false, nil
	}
	source, ok := chooseRefreshSource(last.bundle, uploaded, known)
	if !ok {
		// Neither the bytes nor a recorded reference: nothing to publish
		// against. The next content change publishes current metadata.
		return outcomeSkipped, false, nil
	}
	// Cache before any early return below, so a legacy publication is
	// migrated exactly once rather than re-read on every scan.
	if err := s.published.CacheMetadata(last.encoded); err != nil {
		return outcomeSkipped, false, err
	}
	if sameParser {
		// The same parser over the same retained source yields the same
		// result, including a failed parse: nothing to rebuild.
		return outcomeSkipped, false, nil
	}
	if s.liveTranscriptChanged(last.bundle) {
		// Normal capture is about to publish current-parser metadata with
		// the new content; a metadata-only publication first would only be
		// wasted work that also starts the upload interval early.
		return outcomeSkipped, false, nil
	}
	metadataBytes, changed, err := s.refreshedMetadata(last, source.ref)
	if err != nil || !changed {
		return outcomeSkipped, false, err
	}
	pending := state.PendingPublication{MetadataOnly: true, Bundle: last.bundle, SourceKey: source.ref.Key, MetadataKey: key, SourceSHA256: source.ref.SHA256, SourceBytes: source.bytes, SourceSize: source.ref.CompressedBytes, MetadataBytes: metadataBytes, ReadyAt: s.now, Attempted: true}
	if err := s.local.SavePending(s.id(), pending); err != nil {
		return outcomeSkipped, false, err
	}
	outcome, err = s.publishPending(pending)
	return outcome, true, err
}

// refreshedMetadata derives current-parser metadata for the last
// publication, pointing at source. changed is false when it equals what was
// published apart from its derivation time, or when this build cannot
// derive it at all; the latter is recorded (a refresh skip) so it is not
// retried on every pass and status can count it.
func (s *sessionScan) refreshedMetadata(last lastPublication, source archive.SourceReference) (encoded []byte, changed bool, err error) {
	prior := last.metadata
	next, buildErr := archive.BuildMetadata(last.bundle, s.opts.MachineID, s.reg.SessionStartedAt, s.now, source, archive.ParserInfo{Version: s.opts.parserVersion()})
	next.ApplyRegistrationProvenance(s.reg)
	if buildErr != nil && !archive.IsParseError(buildErr) {
		// This build cannot derive metadata from the retained bundle at all
		// (one cached under an older source schema, say). That is not a
		// failure of the session.
		skip := state.RefreshSkip{ParserVersion: s.opts.parserVersion(), SourceKey: prior.SourceBundle.Key, Reason: state.RefreshSkipUnderivable}
		return nil, false, s.local.SaveRefreshSkip(s.id(), skip)
	}
	// Unchanged metadata over the same source needs no publication.
	comparison := next
	comparison.MetadataDerivedAt = prior.MetadataDerivedAt
	oldBytes, err := json.Marshal(prior)
	if err != nil {
		return nil, false, err
	}
	comparisonBytes, err := json.Marshal(comparison)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(oldBytes, comparisonBytes) {
		return nil, false, nil
	}
	encoded, err = json.Marshal(next)
	return encoded, err == nil, err
}

// lastPublication is what regenerateMetadata refreshes: the last published
// bundle and the metadata document published with it.
type lastPublication struct {
	bundle   archive.SourceBundle
	metadata archive.Metadata
	encoded  []byte
	// legacy means no metadata was cached locally and encoded is the copy
	// read from storage (publications made before metadata was cached).
	legacy bool
}

// lastPublication returns the session's last publication for a refresh. ok
// is false when there is nothing usable to refresh: nothing was ever
// published (a blocked, declined, or rate-limited candidate cached alongside
// it was never made discoverable), or the metadata is unreadable, missing
// from storage, or describes another session, machine, or source.
func (s *sessionScan) lastPublication(metadataKey string) (lastPublication, bool) {
	bundle, _, found := s.published.LastPublished()
	if !found {
		return lastPublication{}, false
	}
	encoded := s.published.Metadata()
	legacy := len(encoded) == 0
	if legacy {
		// One-time migration for publications made before metadata was cached.
		// A missing or unreachable copy is not fatal: nothing can be refreshed
		// from it, and normal capture keeps working without it.
		var err error
		if encoded, err = s.remote.Get(s.ctx, metadataKey); err != nil {
			return lastPublication{}, false
		}
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return lastPublication{}, false
	}
	if metadata.SessionID != s.id() || metadata.MachineID != s.opts.MachineID || metadata.ValidateSourceReference() != nil {
		return lastPublication{}, false
	}
	return lastPublication{bundle: bundle, metadata: metadata, encoded: encoded, legacy: legacy}, true
}

// refreshSource is the source a refreshed metadata document points at, and
// the bytes to publish it with, if this build has them.
type refreshSource struct {
	ref   archive.SourceReference
	bytes []byte
}

// chooseRefreshSource picks the source a metadata refresh points at:
//   - This build reproduces the uploaded source's exact bytes: they are
//     carried, so the source is re-uploaded if it has gone missing.
//   - It builds the retained bundle, but to other bytes (a compressor
//     change): those bytes are published as a new source, which supersedes
//     the old one. The remote is repaired whatever state the old object is
//     in, and from then on the recorded source is one this build reproduces.
//   - It cannot build the bundle at all (a source schema bump): only the
//     recorded reference (uploaded, when known) is carried, and the source is
//     checked in storage against it (see sessionScan.upload).
//
// ok is false when it has neither bytes nor a recorded reference.
func chooseRefreshSource(bundle archive.SourceBundle, uploaded archive.SourceReference, known bool) (refreshSource, bool) {
	if compressed, err := archive.BuildCompressedSource(bundle); err == nil {
		if key, err := archive.SourceObjectKey(bundle, compressed.SHA256); err == nil {
			ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
			return refreshSource{ref: ref, bytes: compressed.Bytes}, true
		}
	}
	return refreshSource{ref: uploaded}, known
}

// liveTranscriptChanged reports whether normal capture will publish this
// scan: the source (a transcript on disk, or a Cursor database chat) carries
// evidence the cached comparison bundle does not, and it still extends what
// capture guards against (the same rules sessionScan.guard applies), so a
// rewrite that capture will only record as a gap does not count. A Cursor
// database chat that was rewritten does count: capture publishes it. It
// compares against the cached candidate rather than only the last
// publication so a declined candidate the transcript still matches leaves
// regeneration free to proceed. A transcript that cannot be compared
// (rotated, oversize, unsafe) reports no change: regeneration is then the
// only way the summary can move.
func (s *sessionScan) liveTranscriptChanged(lastPublished archive.SourceBundle) bool {
	source, ok := newSourceReader(s.reg, s.opts)
	if !ok {
		return false
	}
	cached, _, status, found := s.published.Cached()
	if !found {
		return false
	}
	adapter, err := archive.NewAdapter(s.reg.Harness.Name)
	if err != nil {
		return false
	}
	filtered, _, err := source.Filter(s.ctx, adapter, s.opts.maxTranscriptBytes())
	if err != nil {
		return false
	}
	candidate, err := archive.NewSourceBundle(s.reg, adapter, filtered, s.now, cached.SupplementalEvidence)
	if err != nil {
		return false
	}
	same, err := bundleEvidenceEqual(cached, candidate)
	if err != nil || same {
		return false
	}
	if s.reg.SourceKind == archive.SourceKindCursorSQLite {
		return true
	}
	guard := cached
	if status == state.CacheStatusBlocked {
		guard = lastPublished
	}
	return nativeEvidenceExtends(guard, candidate)
}
