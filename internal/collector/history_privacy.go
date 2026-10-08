package collector

import (
	"errors"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// resumeStricterHistory resolves exact remote status before preparing any
// replacement. Unknown status keeps the attempted descriptor and its evidence.
func (s *sessionScan) resumeStricterHistory(p state.PendingPublication) (sessionOutcome, error) {
	endAttempt, attemptErr := s.beginPublicationAttempt()
	if attemptErr != nil {
		return outcomeSkipped, attemptErr
	}
	defer endAttempt()
	committed, err := s.checkFrozenHistoryMetadata(p)
	if err != nil {
		return outcomeSkipped, err
	}
	if committed {
		if p.History.Preparing {
			return outcomeSkipped, errors.New("remote sidecar matches incomplete private preparation; refuse acknowledgement")
		}
		if p.JournalVersion != 2 {
			legacyPolicy := (state.PublicationPolicy{FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, SkillEvidence: pendingSkillMode(p.SkillEvidence)}).Context()
			prior := s.published.PublicationPredecessor()
			sealed, e := state.PreparePublicationV2(p, prior, s.reg.DestinationID, s.publicationAdmission(), legacyPolicy, state.PublicationCapture)
			if e != nil {
				return outcomeSkipped, e
			}
			if e = s.local.SavePending(s.id(), sealed); e != nil {
				return outcomeSkipped, e
			}
			p = sealed
		}
		if err := s.verifyHistoryReadback(p); err != nil {
			return outcomeSkipped, err
		}
		metadata, err := s.frozenHistoryMetadata(p)
		if err != nil {
			return outcomeSkipped, err
		}
		data, err := s.historyGet(metadata.SourceBundle.Key, int64(metadata.SourceBundle.CompressedBytes))
		if err != nil {
			return outcomeSkipped, err
		}
		p.Bundle, err = s.decodeReferenced(metadata, data)
		if err != nil {
			return outcomeSkipped, err
		}
		// Queue before acknowledging: a crash cannot leave a committed broader set
		// with neither an exact retry nor a durable maintenance obligation.
		if !p.History.MaintenanceOwed {
			p.History.MaintenanceOwed = true
			if p.JournalVersion == 2 {
				if err := s.local.SavePending(s.id(), p); err != nil {
					return outcomeSkipped, err
				}
			}
		}

		if _, err := s.acknowledgePublication(p, true); err != nil {
			return outcomeSkipped, err
		}
	}
	successor, err := s.stricterHistorySuccessor(p, committed)
	if err != nil {
		return outcomeSkipped, err
	}
	// Staging can take several bounded reads. A predecessor that changes during
	// that work cannot authorize replacement of the original retry descriptor.
	stillCommitted, err := s.checkFrozenHistoryMetadata(p)
	if err != nil {
		return outcomeSkipped, err
	}
	if stillCommitted != committed {
		return outcomeSkipped, errHistoryMetadataConflict
	}
	// Every retained input and stricter output is staged before replacement.
	if err := s.savePending(&successor); err != nil {
		return outcomeSkipped, err
	}
	return outcomeSkipped, archive.ErrHistoryMutationPending
}

// stricterHistorySuccessor consumes retained original stages, final sources and
// acknowledged sources sequentially. It never reads a current native source or
// skill inventory, and never assigns maintenance time as a capture time.
func (s *sessionScan) stricterHistorySuccessor(p state.PendingPublication, committed bool) (state.PendingPublication, error) {
	metadata, err := s.frozenHistoryMetadata(p)
	if err != nil {
		return state.PendingPublication{}, err
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return state.PendingPublication{}, err
	}
	inputs := append([]state.HistoryInput(nil), p.History.Inputs...)
	// Older ready journals may lack Inputs. Their bounded validated final bytes
	// supply provenance individually; no active provenance is copied to siblings.
	finalInputs, err := s.retainedManifestInputs(metadata)
	if err != nil {
		return state.PendingPublication{}, err
	}
	for _, input := range finalInputs {
		if !hasRevisionInput(inputs, input.RevisionID) {
			inputs = append(inputs, input)
		}
	}
	acknowledged := archive.Metadata{}
	var acknowledgedBody []byte
	expected := p.History.ExpectedMetadataSHA256
	if committed {
		expected = metadataSHA(p.MetadataBytes)
		acknowledged = metadata
		acknowledgedBody = p.MetadataBytes
	} else if expected != "" {
		raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
		if err != nil {
			return state.PendingPublication{}, err
		}
		acknowledgedBody = raw
		if metadataSHA(raw) != expected {
			return state.PendingPublication{}, errHistoryMetadataConflict
		}
		if err := s.unmarshalRetained(raw, &acknowledged); err != nil {
			return state.PendingPublication{}, err
		}
		if err := s.validateAuthorityIdentity(acknowledged); err != nil {
			return state.PendingPublication{}, err
		}
	}
	var ackInputs []state.HistoryInput
	if acknowledged.SessionID != "" {
		ackInputs, err = s.retainedManifestInputs(acknowledged)
		if err != nil {
			return state.PendingPublication{}, err
		}
		for _, input := range ackInputs {
			if hasRevisionInput(inputs, input.RevisionID) {
				continue
			}
			if len(metadata.History.Preserved) >= archive.MaxHistorySpans {
				return state.PendingPublication{}, errors.New("stricter successor exceeds retained revision limit")
			}
			metadata.History.Preserved = append(metadata.History.Preserved, archive.RevisionReference{RevisionID: input.RevisionID, CapturedAt: input.CapturedAt, Source: input.Reference, FilterVersion: input.FilterVersion, SourceSchemaVersion: input.SourceSchemaVersion})
			inputs = append(inputs, input)
		}
	}
	if len(inputs) > archive.MaxHistorySpans+1 {
		return state.PendingPublication{}, errors.New("stricter successor exceeds input limit")
	}
	next := p
	next.JournalVersion = 0
	next.Phase = ""
	next.Commit = nil
	next.Preparation = nil
	next.Progress = nil
	next.Cleanup = nil
	next.Sources = nil
	// A retained policy successor did not consume native input under this
	// policy. Preserve its pending native-read obligation, never a settled proof.
	next.ScanSignature = nil
	next.History = &state.PendingHistory{Version: 1, Preparing: true, FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), ExpectedMetadataSHA256: expected, Retired: append([]state.RetiredSource(nil), p.History.Retired...)}
	next.Attempted = false
	next.MetadataOnly = false
	next.SkillEvidence = p.SkillEvidence
	// Keep the covered request owed until the stricter selecting successor.

	finalMetadata := metadata
	if err = s.retainCharge(int64(len(metadata.History.Preserved)+1) * 2048); err != nil {
		return state.PendingPublication{}, err
	}
	originalHistory := *metadata.History
	originalHistory.Preserved = append([]archive.RevisionReference(nil), metadata.History.Preserved...)
	metadata.History = &originalHistory
	finalBody := p.MetadataBytes
	// Freeze the chosen original selection before transforming any output.
	// The acknowledged body remains a distinct singleton predecessor authority.
	next.History.Inputs = append([]state.HistoryInput(nil), inputs...)
	for _, input := range inputs {
		if input.RevisionID == metadata.History.CurrentRevision {
			metadata.SourceBundle, metadata.CapturedAt, metadata.FilterVersion = input.Reference, input.CapturedAt, input.FilterVersion
			next.Bundle, err = s.loadHistoryInput(metadata, input)
			if err != nil {
				return state.PendingPublication{}, err
			}
			next.SourceKey, next.SourceSHA256, next.SourceSize = input.Reference.Key, input.Reference.SHA256, input.Reference.CompressedBytes
		} else {
			for i := range metadata.History.Preserved {
				r := &metadata.History.Preserved[i]
				if r.RevisionID == input.RevisionID {
					r.Source, r.CapturedAt, r.FilterVersion, r.SourceSchemaVersion = input.Reference, input.CapturedAt, input.FilterVersion, input.SourceSchemaVersion
				}
			}
		}
		mark := len(s.retainedReleases)
		raw, e := s.readRetainedInputBytes(input.Reference)
		if e != nil {
			return state.PendingPublication{}, e
		}
		_, e = s.local.StagePublicationSource(s.id(), input.Reference, raw)
		s.releaseRetainedAfter(mark)
		if e != nil {
			return state.PendingPublication{}, e
		}
	}
	next.SourceBytes = nil
	next.MetadataBytes, err = s.marshalRetained(metadata)
	if err != nil {
		return state.PendingPublication{}, err
	}
	prior := state.PublicationPredecessor{State: state.PredecessorAbsent}
	if expected != "" {
		prior.State, prior.Body = state.PredecessorPresent, acknowledgedBody
		prior.Bundle, err = s.loadHistoryInput(acknowledged, ackInputs[0])
		if err != nil {
			return state.PendingPublication{}, err
		}
	}
	var hookObservations []archive.SupplementalEvidence
	for _, item := range p.Bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindLinkedSession || item.Kind == archive.EvidenceKindExplicitFeedback {
			hookObservations = append(hookObservations, item)
		}
	}
	if err := s.freezePublicationHooks(&next, hookObservations, adapter.Version()); err != nil {
		return state.PendingPublication{}, err
	}
	next, err = state.PreparePublicationV2(next, prior, s.reg.DestinationID, s.publicationAdmission(), storage.SHA256Hex([]byte(archive.FilterVersion+"\x00"+adapter.Version()+"\x00"+string(s.opts.skillEvidence()))), state.PublicationPrivacyRewrite)
	if err != nil {
		return state.PendingPublication{}, err
	}
	next.SkillEvidence = string(s.opts.skillEvidence())
	var total int
	for index, input := range inputs {
		mark := len(s.retainedReleases)
		filtered, ref, stage, receiptRelease, err := s.prepareStricterHistoryInput(&next, index, metadata, input, finalMetadata, finalBody, finalInputs, acknowledged, acknowledgedBody, ackInputs, adapter, pendingSkillMode(p.SkillEvidence), &total)
		if err != nil {
			return state.PendingPublication{}, err
		}

		// Use the chosen original's immutable capture provenance throughout.
		if input.RevisionID == metadata.History.CurrentRevision {
			metadata.SourceBundle, metadata.CapturedAt, metadata.FilterVersion = input.Reference, input.CapturedAt, input.FilterVersion
		} else {
			for i := range metadata.History.Preserved {
				r := &metadata.History.Preserved[i]
				if r.RevisionID == input.RevisionID {
					r.Source, r.CapturedAt = input.Reference, input.CapturedAt
				}
			}
		}
		data, err := s.historyStage(stage)
		if err != nil {
			return state.PendingPublication{}, err
		}
		if err := replacePreparedReference(&next, &metadata, input, filtered, ref, data); err != nil {
			return state.PendingPublication{}, err
		}
		next.History.Sources = append(next.History.Sources, stage)
		s.releaseStricterAlternative(mark, input.RevisionID, metadata.History.CurrentRevision)
		s.retainedReleases = append(s.retainedReleases, receiptRelease)
	}
	if err := retireStricterHistoryReferences(&next, metadata, append(append(append([]state.HistoryInput(nil), inputs...), finalInputs...), ackInputs...), s.now); err != nil {
		return state.PendingPublication{}, err
	}
	next.History.Preparing = false
	next.History.PrivacyCursor = len(next.History.Inputs)
	next.History.PreparedAt = s.now
	if err := s.derivePreparedHistory(&next, metadata); err != nil {
		return state.PendingPublication{}, err
	}
	var derived archive.Metadata
	if err := s.unmarshalRetained(next.MetadataBytes, &derived); err != nil {
		return state.PendingPublication{}, err
	}
	if err := boundFrozenReferences(derived); err != nil {
		return state.PendingPublication{}, err
	}
	next.JournalVersion = 2
	next.Phase = "ready"
	next, err = state.PreparePublicationV2(next, prior, s.reg.DestinationID, s.publicationAdmission(), next.Preparation.PolicyContext, state.PublicationPrivacyRewrite)
	if err != nil {
		return state.PendingPublication{}, err
	}
	return next, next.ValidateHistoryBudgeted(s.id(), s.readBudget())
}

func hasRevisionInput(inputs []state.HistoryInput, id string) bool {
	for _, input := range inputs {
		if input.RevisionID == id {
			return true
		}
	}
	return false
}

func (s *sessionScan) retainedManifestInputs(metadata archive.Metadata) ([]state.HistoryInput, error) {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	if err := s.validateAuthorityIdentity(metadata); err != nil {
		return nil, err
	}
	if err := boundFrozenReferences(metadata); err != nil {
		return nil, err
	}
	refs, err := metadata.SourceReferences()
	if err != nil {
		return nil, err
	}
	inputs := make([]state.HistoryInput, 0, len(refs))
	for i, ref := range refs {
		mark := len(s.retainedReleases)
		id := metadata.NativeSessionID
		if metadata.History != nil {
			id = metadata.History.CurrentRevision
		}
		if i > 0 {
			id = metadata.History.Preserved[i-1].RevisionID
		}
		data, err := s.historyStage(state.PendingSource{Reference: ref, Name: ref.SHA256 + ".gz"})
		if err != nil {
			data, err = s.historyGet(ref.Key, int64(ref.CompressedBytes))
		}
		if err != nil {
			return nil, err
		}
		var bundle archive.SourceBundle
		if metadata.History == nil {
			bundle, err = s.decodeReferenced(metadata, data)
		} else {
			bundle, err = s.decodeRevision(metadata, id, data)
		}
		if err != nil {
			return nil, fmt.Errorf("validate retained successor input: %w", err)
		}
		inputs = append(inputs, state.HistoryInput{Reference: ref, RevisionID: id, CapturedAt: bundle.Capture.CapturedAt, FilterVersion: bundle.Capture.FilterVersion, SourceSchemaVersion: bundle.SchemaVersion})
		s.releaseRetainedAfter(mark)
	}
	return inputs, nil
}

// prepareRetainedHistoryWork runs before the temporary mutation fence, enabling
// only private preparation and exact already-committed acknowledgement. Actual
// publication, retention and generation changes still require checkpoint 5.
func (s *sessionScan) prepareRetainedHistoryWork() (sessionOutcome, bool, error) {
	if len(s.published.Metadata()) == 0 {
		return outcomeSkipped, false, nil
	}
	var metadata archive.Metadata
	if err := s.unmarshalRetained(s.published.Metadata(), &metadata); err != nil {
		return outcomeSkipped, false, err
	}
	if metadata.History == nil {
		return outcomeSkipped, false, nil
	}
	if err := s.validateAuthorityIdentity(metadata); err != nil {
		return outcomeSkipped, true, err
	}
	if s.reg.CaptureFrozen {
		if err := s.local.FrozenGeneration(s.reg); err != nil {
			return outcomeSkipped, true, err
		}
	} else if err := s.local.GenerationCaptureAllowed(s.reg); err != nil {
		return outcomeSkipped, true, err
	}
	if p, found, err := s.local.LoadPublicationPending(s.id()); err != nil {
		return outcomeSkipped, true, err
	} else if found {
		if p.History == nil {
			return outcomeSkipped, true, errors.New("ordinary pending journal cannot replace retained history")
		}
		outcome, err := s.resumeHistory(p)
		if err == nil && s.reg.CaptureFrozen {
			err = s.recordFrozenSignature()
		}
		return outcome, true, err
	}
	// Strict acknowledged authority is independent of ordinary stale-filter
	// compatibility. Each input is decoded before constructing this journal.
	authority, found, err := s.published.LastPublishedMetadata()
	if err != nil || !found {
		return outcomeSkipped, true, errors.Join(errors.New("complete acknowledged history authority required for maintenance"), err)
	}
	bundle, _, found := s.published.LastPublished()
	if !found {
		return outcomeSkipped, true, errors.New("acknowledged history bundle is missing")
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, true, err
	}
	needed := bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) || authority.Parser.Version != s.parserVersion() || s.reg.CaptureFrozen && s.req.Token != "" || headFingerprint(s.reg.LastHead) != s.publishedLastHead()
	for _, revision := range authority.History.Preserved {
		needed = needed || revision.FilterVersion != archive.FilterVersion
	}
	if !needed {
		return outcomeSkipped, false, nil
	}
	p, err := s.freezeRetainedMaintenance(authority, bundle, adapter.Version())
	if err != nil {
		return outcomeSkipped, true, err
	}
	// Prepare one retained ref per pass, freezing policy in the descriptor.
	if err := s.advanceHistoryPreparation(&p); err != nil {
		return outcomeSkipped, true, err
	}
	return outcomeSkipped, true, archive.ErrHistoryMutationPending
}

func (s *sessionScan) prepareStricterHistoryInput(next *state.PendingPublication, index int, metadata archive.Metadata, input state.HistoryInput, finalMetadata archive.Metadata, finalBody []byte, finalInputs []state.HistoryInput, acknowledged archive.Metadata, acknowledgedBody []byte, ackInputs []state.HistoryInput, adapter agentapi.TranscriptFilter, ceiling config.SkillEvidence, total *int) (archive.SourceBundle, archive.SourceReference, state.PendingSource, func(), error) {
	frozen := next.Preparation.Inputs[index]
	reader := publicationPrivacyReader{scan: s}
	raw, closeBytes, err := reader.ReadPublicationPrivacySource(s.ctx, frozen)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	defer closeBytes()
	if len(raw) > (128<<20)-*total {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, storage.ErrObjectTooLarge
	}
	*total += len(raw)
	var alternatives []state.PublicationPrivacyAlternative
	add := func(owner archive.Metadata, body []byte, others []state.HistoryInput) {
		for _, other := range others {
			if other.RevisionID != input.RevisionID || other.Reference == input.Reference {
				continue
			}
			alt := state.PreparationInput{Reference: other.Reference, Selection: state.PublicationSelection{Role: frozen.Selection.Role, RevisionID: other.RevisionID, CapturedAt: other.CapturedAt.UTC(), SourceSchemaVersion: other.SourceSchemaVersion}, FilterVersion: other.FilterVersion, AdapterVersion: owner.Adapter.Version, SkillPolicy: string(ceiling)}
			policy := state.PublicationPolicy{FilterVersion: alt.FilterVersion, AdapterVersion: alt.AdapterVersion, SkillEvidence: ceiling}
			alternatives = append(alternatives, state.PublicationPrivacyAlternative{Metadata: owner, MetadataBody: body, Input: alt, Policy: policy})
		}
	}
	add(finalMetadata, finalBody, finalInputs)
	add(acknowledged, acknowledgedBody, ackInputs)
	var origin archive.Metadata
	if err = s.unmarshalRetained(next.Preparation.OriginMetadata, &origin); err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	filtered, packed, ref, proof, closeFactory, err := state.RefilterCoveredPublicationInput(s.ctx, s.reg, adapter, origin, next.Preparation.OriginMetadata, frozen, index, raw, state.PublicationContext{DestinationID: s.reg.DestinationID, AdmissionContext: s.publicationAdmission()}, state.PublicationPolicy{FilterVersion: frozen.FilterVersion, AdapterVersion: frozen.AdapterVersion, SkillEvidence: pendingSkillMode(frozen.SkillPolicy)}, state.PublicationPolicy{FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), SkillEvidence: s.opts.skillEvidence()}, ceiling, s.readBudget(), reader, alternatives)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	// The caller promotes the actual factory output lease before ending input.
	s.retainedReleases = append(s.retainedReleases, closeFactory)
	if len(packed.Bytes) > (128<<20)-*total {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, storage.ErrObjectTooLarge
	}
	*total += len(packed.Bytes)
	stage, err := s.local.StagePublicationSource(s.id(), ref, packed.Bytes)
	if err != nil {
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	receiptRelease, recordErr := next.RecordPrivacyOutput(proof)
	if recordErr != nil {
		err = recordErr
		return archive.SourceBundle{}, archive.SourceReference{}, state.PendingSource{}, nil, err
	}
	return filtered, ref, stage, receiptRelease, nil
}

func (s *sessionScan) freezeRetainedMaintenance(authority archive.Metadata, bundle archive.SourceBundle, adapterVersion string) (state.PendingPublication, error) {
	inputs, err := s.retainedManifestInputs(authority)
	if err != nil {
		return state.PendingPublication{}, err
	}
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return state.PendingPublication{}, err
	}
	var observations []archive.SupplementalEvidence
	for _, evidence := range s.req.HookEvidence {
		if evidence.Kind == archive.EvidenceKindLinkedSession || evidence.Kind == archive.EvidenceKindExplicitFeedback {
			observations = append(observations, evidence)
		}
	}
	bundle.SupplementalEvidence = mergeSupplementalEvidence(bundle.SupplementalEvidence, observations)
	// Live requests still owe native reads; frozen requests cover retained observations.
	requestToken := ""
	if s.reg.CaptureFrozen {
		requestToken = s.req.Token
	}
	p := state.PendingPublication{SkillEvidence: string(s.opts.skillEvidence()), Bundle: bundle, MetadataKey: key, MetadataBytes: append([]byte(nil), s.published.Metadata()...), RequestToken: requestToken, ReadyAt: s.now, History: &state.PendingHistory{Version: 1, Preparing: true, Inputs: inputs, FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion, ExpectedMetadataSHA256: metadataSHA(s.published.Metadata())}}
	if err := s.local.SweepPendingSources(s.id()); err != nil {
		return state.PendingPublication{}, err
	}
	for _, input := range inputs {
		mark := len(s.retainedReleases)
		raw, err := s.readRetainedInputBytes(input.Reference)
		if err != nil {
			return state.PendingPublication{}, err
		}
		stage, err := s.local.StagePublicationSource(s.id(), input.Reference, raw)
		if err != nil {
			return state.PendingPublication{}, err
		}
		p.History.Sources = append(p.History.Sources, stage)
		if input.RevisionID == authority.History.CurrentRevision {
			p.SourceKey, p.SourceSHA256, p.SourceBytes = input.Reference.Key, input.Reference.SHA256, raw
		} else {
			s.releaseRetainedAfter(mark)
		}
	}
	// Same-policy observations are a new capture selection, normalized before
	// the immutable preparation freezes its inputs. Privacy transformations keep
	// the exact old input and require their factory-owned transformation receipt.
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return state.PendingPublication{}, err
	}
	for i, input := range p.History.Inputs {
		if input.RevisionID != authority.History.CurrentRevision {
			continue
		}
		if bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
			break
		}
		if err := s.prepareHistoryInput(&p, &authority, input); err != nil {
			return state.PendingPublication{}, err
		}
		p.History.Inputs[i] = state.HistoryInput{Reference: authority.SourceBundle, RevisionID: input.RevisionID, CapturedAt: input.CapturedAt, FilterVersion: p.Bundle.Capture.FilterVersion, SourceSchemaVersion: p.Bundle.SchemaVersion}
		refs, err := authority.SourceReferences()
		if err != nil {
			return state.PendingPublication{}, err
		}
		stages := p.History.Sources[:0]
		for _, stage := range p.History.Sources {
			for _, ref := range refs {
				if stage.Reference == ref {
					stages = append(stages, stage)
					break
				}
			}
		}
		p.History.Sources = stages
		p.MetadataBytes, err = s.marshalRetained(authority)
		if err != nil {
			return state.PendingPublication{}, err
		}
		break
	}
	if bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
		if err := s.freezePublicationHooks(&p, observations, adapter.Version()); err != nil {
			return state.PendingPublication{}, err
		}
	}
	if err := s.savePending(&p); err != nil {
		return state.PendingPublication{}, err
	}
	return p, nil
}

func retireStricterHistoryReferences(next *state.PendingPublication, metadata archive.Metadata, inputs []state.HistoryInput, at time.Time) error {
	// Keep every obsolete final/acknowledged/input cleanup obligation, including
	// objects prepared but not uploaded. Protection is by the complete final set.
	protected, err := metadata.SourceReferences()
	if err != nil {
		return err
	}
	for _, input := range inputs {
		live := false
		for _, ref := range protected {
			live = live || ref == input.Reference
		}
		if live {
			continue
		}
		found := false
		for i := range next.History.Retired {
			if next.History.Retired[i].Reference == input.Reference {
				next.History.Retired[i].PrivacySensitive = true
				found = true
			}
		}
		if !found {
			next.History.Retired = append(next.History.Retired, state.RetiredSource{Reference: input.Reference, RetiredAt: at, PrivacySensitive: true})
		}
	}
	// Remove reactivated refs from inherited cleanup obligations.
	retired := next.History.Retired[:0]
	for _, old := range next.History.Retired {
		live := false
		for _, ref := range protected {
			live = live || old.Reference == ref
		}
		if !live {
			if old.RetiredAt.IsZero() {
				old.RetiredAt = at
			}
			retired = append(retired, old)
		}
	}
	next.History.Retired = retired
	return nil
}

func (s *sessionScan) releaseStricterAlternative(mark int, revision, current string) {
	if revision != current {
		s.releaseRetainedAfter(mark)
	}
}

// freezePublicationHooks consumes only the owned request/candidate facts before
// the first preparation; later slices cannot consult newer request observations.
func (s *sessionScan) freezePublicationHooks(p *state.PendingPublication, observations []archive.SupplementalEvidence, adapterVersion string) error {
	if len(observations) == 0 {
		return nil
	}
	raw, err := s.marshalRetained(observations)
	if err != nil {
		return err
	}
	policy := state.PublicationPolicy{FilterVersion: archive.FilterVersion, AdapterVersion: adapterVersion, SkillEvidence: s.opts.skillEvidence()}
	return state.FreezePublicationHookObservations(s.ctx, p, raw, s.reg.DestinationID, s.publicationAdmission(), policy.Context(), s.readBudget())
}
