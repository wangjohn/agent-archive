package collector

import (
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// freezeRevisionPublication stages reconciled evidence before persisting its
// descriptor. Only acknowledged complete authority supplies the predecessor.
func (s *sessionScan) freezeRevisionPublication(p *state.PendingPublication) error {
	var final archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &final); err != nil {
		return err
	}
	final.SchemaVersion = archive.HistoryMetadataSchemaVersion
	final.History = &archive.RevisionHistory{CurrentRevision: s.revisions.Current, Preserved: append([]archive.RevisionReference(nil), s.revisions.Preserved...)}
	previous, found, err := s.published.LastPublishedMetadata()
	if err != nil {
		return err
	}
	predecessor := ""
	if found {
		if err := s.validateAuthorityIdentity(previous); err != nil {
			return err
		}
		predecessor = metadataSHA(s.published.Metadata())
	} else {
		// Absence must be observed, never inferred from missing local state.
		if _, err := historyLimitedGet(s.ctx, s.remote, p.MetadataKey, historyMetadataLimit); !errors.Is(err, storage.ErrNotFound) {
			if err != nil {
				return err
			}
			return errHistoryMetadataConflict
		}
	}
	history := &state.PendingHistory{Version: 1, Preparing: true, FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, ExpectedMetadataSHA256: predecessor}
	if err := boundFrozenReferences(final); err != nil {
		return err
	}
	total := len(p.SourceBytes)
	if total > 128<<20 {
		return storage.ErrObjectTooLarge
	}
	for _, source := range s.revisions.Sources {
		if len(source.Bytes) > (128<<20)-total {
			return storage.ErrObjectTooLarge
		}
		total += len(source.Bytes)
	}
	// Sweep only before staging begins: live descriptor and input stages survive.
	if err := s.local.SweepPendingSources(s.id()); err != nil {
		return err
	}
	active, err := s.local.StagePendingSource(s.id(), p.SourceReference(), p.SourceBytes)
	if err != nil {
		return err
	}
	history.Sources = append(history.Sources, active)
	for _, source := range s.revisions.Sources {
		stage, err := s.local.StagePendingSource(s.id(), source.Reference, source.Bytes)
		if err != nil {
			return err
		}
		history.Sources = append(history.Sources, stage)
	}
	history.Inputs = append(history.Inputs, state.HistoryInput{Reference: p.SourceReference(), RevisionID: s.revisions.Current, CapturedAt: p.Bundle.Capture.CapturedAt, FilterVersion: p.Bundle.Capture.FilterVersion, SourceSchemaVersion: p.Bundle.SchemaVersion})
	inputs := s.revisions.historyInputs()
	for i := range final.History.Preserved {
		revision := &final.History.Preserved[i]
		if revision.FilterVersion == "" || revision.SourceSchemaVersion == 0 {
			raw, err := historyLimitedGet(s.ctx, s.remote, revision.Source.Key, int64(revision.Source.CompressedBytes))
			if err != nil {
				return err
			}
			bundle, err := reader.DecodeRevisionSource(s.ctx, final, revision.RevisionID, raw, reader.Limits{})
			if err != nil {
				return err
			}
			revision.FilterVersion, revision.SourceSchemaVersion = bundle.Capture.FilterVersion, bundle.SchemaVersion
		}
		inputs[i].FilterVersion, inputs[i].SourceSchemaVersion = revision.FilterVersion, revision.SourceSchemaVersion
		history.Inputs = append(history.Inputs, inputs[i])
	}
	if found {
		refs, err := previous.SourceReferences()
		if err != nil {
			return err
		}
		finalRefs, err := final.SourceReferences()
		if err != nil {
			return err
		}
		protected := map[archive.SourceReference]bool{}
		for _, ref := range finalRefs {
			protected[ref] = true
		}
		for _, ref := range refs {
			if !protected[ref] {
				history.Retired = append(history.Retired, state.RetiredSource{Reference: ref})
			}
		}
	}
	p.MetadataBytes, err = json.Marshal(final)
	if err != nil {
		return err
	}
	p.History, p.Attempted = history, false
	return p.ValidateHistory(s.id())
}

// resumeHistory never bypasses retained work for a newer request or policy.
// A policy mismatch stays pending until complete successor preparation can
// resolve the remote prior/final state without uploading broader bytes.
func (s *sessionScan) resumeHistory(p state.PendingPublication) (sessionOutcome, error) {
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, err
	}
	if pendingSkillMode(p.SkillEvidence) != s.opts.skillEvidence() || p.Bundle.Capture.FilterVersion != archive.FilterVersion || p.Bundle.Capture.AdapterVersion != adapter.Version() {
		return outcomeSkipped, errors.New("history privacy policy changed; retain frozen evidence for all-reference successor preparation")
	}
	if err := s.recoverHistoryStages(p); err != nil {
		return outcomeSkipped, err
	}
	if err := s.advanceHistoryPreparation(&p); err != nil {
		return outcomeSkipped, err
	}
	if p.History.Preparing {
		return outcomeSkipped, archive.ErrHistoryMutationPending
	}
	if latest := s.now.Add(s.opts.minUploadInterval()); p.ReadyAt.After(latest) {
		p.ReadyAt = latest
		if err := s.local.SavePending(s.id(), p); err != nil {
			return outcomeSkipped, err
		}
	}
	if s.now.Before(p.ReadyAt) {
		s.readyAt = p.ReadyAt
		return outcomeRateLimited, nil
	}
	return s.publishPending(p)
}

func boundFrozenReferences(final archive.Metadata) error {
	refs, err := final.SourceReferences()
	if err != nil {
		return err
	}
	total := 0
	for _, ref := range refs {
		if ref.CompressedBytes <= 0 || int64(ref.CompressedBytes) > historyCompressedLimit || ref.CompressedBytes > (128<<20)-total {
			return storage.ErrObjectTooLarge
		}
		total += ref.CompressedBytes
	}
	return nil
}

// guardRevisionCandidate retains same-physical extension and age semantics before
// the ordinary guard is bypassed for a proved physical transition.
func (s *sessionScan) guardRevisionCandidate(read sourceRead, candidate *archive.SourceBundle) error {
	prior, _, found := s.published.LastPublished()
	if cached, _, status, present := s.published.Cached(); present && status != state.CacheStatusBlocked {
		prior, found = cached, true
	}
	if !found || revisionID(prior) != revisionID(*candidate) {
		return nil
	}
	if !ownedEvidenceCovered(read.adapter, prior, *candidate) {
		return errors.New("current physical revision no longer extends verified evidence; retain history pending reconciliation")
	}
	if ownedEvidenceCovered(read.adapter, *candidate, prior) {
		candidate.Capture.CapturedAt = prior.Capture.CapturedAt
	}
	return nil
}
