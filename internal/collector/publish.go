package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// blockThenRemember is block for a size limit, which for a Cursor chat also
// remembers the state it was reached at (see rememberFailedRead).
func (s *sessionScan) blockThenRemember(reason state.BlockedReason, read sourceRead) (sessionOutcome, error) {
	outcome, err := s.block(reason, nil, &read.observed)
	if err != nil {
		return outcome, err
	}
	return outcome, rememberFailedRead(s.local, s.reg, read.adapter, read.observed, s.opts, nil, reason, s.publishedLastHead())
}

// block records a terminal capture gap for the session and completes its
// request, so the pass ends cleanly and the session is not counted as
// pending. candidate, when known, becomes the cached comparison bundle so an
// unchanged transcript is skipped on the next pass; otherwise the previously
// cached bundle (or the last published one) is kept. The last published
// snapshot is retained untouched either way. observed is the source state the
// gap was reached at, which the next pass compares against to skip the
// session while it stands (nil: not known, so it is scanned again).
func (s *sessionScan) block(reason state.BlockedReason, candidate *archive.SourceBundle, observed *sourceState) (sessionOutcome, error) {
	s.gap = reason
	cached, _, _, haveCached := s.published.Cached()
	lastPublished, lastPublishedAt, _ := s.published.LastPublished()
	bundle := lastPublished
	if haveCached {
		bundle = cached
	}
	if candidate != nil {
		bundle = *candidate
	}
	previousReason, blocked := s.published.Blocked()
	alreadyBlocked := blocked && previousReason == reason && candidate == nil
	// A block that can end holds on to the hook evidence its acknowledgement
	// would otherwise discard, so a final response that arrived just before
	// the application deleted its transcript still publishes if the file
	// comes back. A permanent block has nothing to hand it back to.
	var held []archive.SupplementalEvidence
	if reason.Recoverable() {
		held = s.req.HookEvidence
	}
	if !alreadyBlocked || len(held) > 0 {
		if err := s.published.SaveBlocked(bundle, lastPublishedAt, reason, held...); err != nil {
			return outcomeSkipped, fmt.Errorf("cache blocked session: %w", err)
		}
	}
	// The gap stands until the source changes from the state it was reached
	// at, a missing transcript's absence included (one that comes back is
	// read at once): until then each pass skips it on a stat, rather than
	// decoding its published state and journaling a scan to reach the same
	// gap again.
	if err := s.recordBlockedSignature(reason, observed); err != nil {
		return outcomeSkipped, err
	}
	return outcomeSkipped, s.completeRequest("complete blocked request")
}

// publishPending uploads a pending publication and records it: source then
// metadata (or, for a metadata-only publication that carries no source
// bytes, a check of the recorded source then metadata), the superseded
// source in the ledger, the new published state, the covered request, and
// finally the removal of the pending file.
func (s *sessionScan) publishPending(pending state.PendingPublication) (sessionOutcome, error) {
	if err := s.checkHistoryPublication(pending); err != nil {
		return outcomeSkipped, err
	}
	if err := s.sealPending(&pending); err != nil {
		return outcomeSkipped, err
	}
	if err := s.checkPrivacyInputReplay(pending); err != nil {
		return outcomeSkipped, err
	}
	// Marking it attempted rewrites the whole file, source bytes and bundle
	// included, so it is done once: a retry of an attempted publication, or
	// one saved already marked because it was due at once, skips it.
	if !pending.Attempted {
		pending.Attempted = true
		if err := s.savePending(&pending); err != nil {
			return outcomeSkipped, fmt.Errorf("mark pending publication attempted: %w", err)
		}
	}
	if err := s.upload(pending); err != nil {
		return outcomeSkipped, err
	}
	// Auxiliary repair always follows the authoritative winner, never an old journal body.
	body, err := storage.ReadPublicationMetadata(s.ctx, s.remote, pending.MetadataKey)
	if err != nil {
		return outcomeSkipped, err
	}
	if storage.SHA256Hex(body) != pending.Commit.MetadataSHA256 {
		// Leave repair journaled; the repair path verifies the winner's complete set.
		return outcomeSkipped, storage.ErrPublicationConflict
	}
	listingPublished := false
	if err := listingindex.PublishRevision(s.ctx, s.remote, pending.MetadataKey, body); err != nil {
		s.warn(fmt.Errorf("listing maintenance pending: %w", err))
	} else {
		listingPublished = true
	}
	// Record only references absent from the complete next set. Preserved revisions
	// stay live; privacy-sensitive predecessor ledger failure keeps replay evidence.
	priorBundle, _, havePrior := s.published.LastPublished()
	previous, err := s.published.CommittedSources()
	if err != nil {
		return outcomeSkipped, err
	}
	for _, ref := range pending.PrivacyRetiredSources() {
		if !slices.Contains(previous, ref) {
			previous = append(previous, ref)
		}
	}
	selected := map[string]bool{}
	for _, source := range pending.Sources {
		selected[source.Reference.Key] = true
	}
	for _, previous := range previous {
		if selected[previous.Key] {
			continue
		}
		privacySensitive := pending.Commit.Purpose == state.PublicationPrivacyRewrite || havePrior && priorBundle.Capture.FilterVersion != pending.Bundle.Capture.FilterVersion
		if err := s.local.RecordSupersededWithPrivacy(s.id(), previous.Key, s.now, privacySensitive); err != nil {
			if privacySensitive {
				return outcomeSkipped, fmt.Errorf("record privacy-sensitive predecessor: %w", err)
			}
			s.warn(fmt.Errorf("record superseded source for cleanup: %w", err))
		}
	}
	// Verify once more after listing/ledger work before committing local authority.
	body, err = storage.ReadPublicationMetadata(s.ctx, s.remote, pending.MetadataKey)
	if err != nil {
		return outcomeSkipped, err
	}
	if storage.SHA256Hex(body) != pending.Commit.MetadataSHA256 {
		return outcomeSkipped, storage.ErrPublicationConflict
	}
	if err := s.published.SaveCommittedPublication(pending, s.now); err != nil {
		return outcomeSkipped, fmt.Errorf("update published cache: %w", err)
	}
	if listingPublished {
		if err := s.local.RemoveListingRepair(s.id()); err != nil {
			s.warn(err)
		}
	}
	// Whatever kept this session's metadata from being refreshed described
	// the publication just replaced.
	if err := s.local.RemoveRefreshSkip(s.id()); err != nil {
		return outcomeSkipped, err
	}
	if pending.RequestToken != "" {
		if _, err := s.local.CompleteRequest(s.id(), pending.RequestToken); err != nil {
			return outcomeSkipped, fmt.Errorf("complete published request: %w", err)
		}
	}
	if err := s.releasePublishedStage(pending); err != nil {
		return outcomeSkipped, err
	}

	if err := s.cleanupPrivacyInput(pending); err != nil {
		return outcomeSkipped, err
	}
	if err := s.local.RemovePending(s.id()); err != nil {
		return outcomeSkipped, err
	}
	return outcomePublished, nil
}

// upload writes a pending publication to storage.
func (s *sessionScan) upload(pending state.PendingPublication) error {
	if err := s.local.SaveListingRepair(s.id(), state.ListingRepair{MetadataKey: pending.MetadataKey, DestinationID: s.reg.DestinationID}); err != nil {
		return fmt.Errorf("journal listing repair: %w", err)
	}
	sources := make([]storage.SourcePublication, len(pending.Sources))
	for i, source := range pending.Sources {
		payload := source.Bytes
		if i == 0 {
			payload = pending.SourceBytes
		}
		sources[i] = storage.SourcePublication{Key: source.Reference.Key, SHA256: source.Reference.SHA256, Size: source.Reference.CompressedBytes, Bytes: payload}
	}
	prior := storage.MetadataPredecessor{Known: pending.Commit.Predecessor != state.PredecessorUnknown, Exists: pending.Commit.Predecessor == state.PredecessorPresent, SHA256: pending.Commit.PredecessorSHA256}
	if err := storage.PutSourceSetThenMetadata(s.ctx, s.remote, sources, pending.MetadataKey, pending.MetadataBytes, prior, s.opts.Retry); err != nil {
		return fmt.Errorf("publish exact source set: %w", err)
	}
	return nil
}

// sealPending upgrades legacy replay only from retained local committed evidence.
func (s *sessionScan) sealPending(p *state.PendingPublication) error {
	policy, err := s.publicationPolicy(p.Bundle)
	if err != nil {
		return err
	}
	if p.Commit != nil {
		if p.Commit.DestinationID != s.reg.DestinationID || p.Commit.PolicyContext != policy || p.Commit.AdmissionContext != s.publicationAdmission() {
			return errors.New("publication destination, admission or filter policy changed; retain pending evidence and reconcile")
		}
		return p.ValidatePublication()
	}
	prior := s.published.PublicationPredecessor()
	if err := s.bindPublicationContinuity(&prior, *p); err != nil {
		return err
	}
	purpose := state.PublicationCapture
	if p.MetadataOnly {
		purpose = state.PublicationMetadata
	}
	sealed, err := state.PreparePublication(*p, prior, s.reg.DestinationID, s.publicationAdmission(), policy, purpose)
	if err != nil {
		return fmt.Errorf("prepare publication: %w", err)
	}
	// Persist the sealed predecessor before any remote write, including legacy replay.
	if err := s.local.SavePending(s.id(), sealed); err != nil {
		return err
	}
	*p = sealed
	return nil
}

func (s *sessionScan) savePending(p *state.PendingPublication) error {
	if p.Commit == nil {
		return s.sealPending(p)
	}
	return s.local.SavePending(s.id(), *p)
}

func (s *sessionScan) publicationPolicy(bundle archive.SourceBundle) (string, error) {
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return "", fmt.Errorf("active publication privacy policy unavailable: %w", err)
	}
	if bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() {
		return "", errors.New("pending source privacy policy changed; retain evidence and refilter before publication")
	}
	return storage.SHA256Hex([]byte(archive.FilterVersion + "\x00" + adapter.Version() + "\x00" + string(s.opts.skillEvidence()))), nil
}

func (s *sessionScan) publicationAdmission() string {
	if s.reg.AdmissionStage != "" {
		return state.AdmissionStageContext(s.reg)
	}
	body, _ := json.Marshal(struct {
		Session   string                `json:"Session"`
		Native    string                `json:"Native"`
		Project   string                `json:"Project"`
		Admission string                `json:"Admission"`
		Origin    archive.SessionOrigin `json:"Origin"`
		Batch     archive.ImportBatch   `json:"Batch"`
	}{Session: s.reg.ArchiveSessionID, Native: s.reg.NativeSessionID, Project: s.reg.ProjectID, Admission: s.reg.Admitted().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), Origin: s.reg.Origin, Batch: s.reg.ImportBatch})
	return storage.SHA256Hex(body)
}

// bindPublicationContinuity consults only the injected native retained comparator.
// The proof certifies filtered continuation, never raw dependency availability.
func (s *sessionScan) bindPublicationContinuity(prior *state.PublicationPredecessor, pending state.PendingPublication) error {
	if prior.State != state.PredecessorPresent || prior.Bundle.History == nil || pending.Bundle.History == nil || prior.Bundle.History.ActiveRolloutID != pending.Bundle.History.ActiveRolloutID {
		return nil
	}
	var previous archive.Metadata
	if err := json.Unmarshal(prior.Body, &previous); err != nil {
		return err
	}
	if previous.SourceBundle.SHA256 == pending.SourceSHA256 {
		return nil
	}
	a, b := prior.Bundle.Capture, pending.Bundle.Capture
	if a.FilterVersion != b.FilterVersion || a.AdapterVersion != b.AdapterVersion || a.SourceFormat != b.SourceFormat || a.AdapterName != b.AdapterName {
		return errors.New("same revision continuation requires matching filter and codec")
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return err
	}
	if a.FilterVersion != archive.FilterVersion || a.AdapterVersion != adapter.Version() || !adapter.EvidenceExtends(prior.Bundle, pending.Bundle) {
		return errors.New("native retained comparator refused same revision continuation")
	}
	prior.SameRevisionContinuity = &state.PublicationContinuity{PreviousSourceSHA256: previous.SourceBundle.SHA256, NextSourceSHA256: pending.SourceSHA256}
	return nil
}

func (s *sessionScan) releasePublishedStage(pending state.PendingPublication) error {
	if pending.AdmissionStage != "" {
		released, err := s.local.AdmissionStageReleased(s.reg)
		if err != nil {
			return err
		}
		if !released {
			resumed, err := s.local.ResumeAdmissionStageRelease(s.reg, s.published)
			if err != nil {
				return err
			}
			if !resumed {
				manifest, _, err := s.local.ReadAdmissionStage(s.id(), pending.AdmissionStage)
				if err != nil {
					return err
				}
				if err = s.local.ReleaseAdmissionStage(s.reg, manifest, s.published, pending.RequestToken); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
