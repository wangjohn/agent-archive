package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// PrepareGenerationRecovery filters the current admitted file without reading
// or rewriting retained history. The returned builder renders a fixed snapshot
// with the latest locked registration and its explicit successor identity.
func PrepareGenerationRecovery(ctx context.Context, reg archive.SessionRegistration, at time.Time, opts Options) (func(archive.SessionRegistration, string) (archive.SessionRegistration, state.PendingPublication, error), error) {
	if reg.ParentSessionID != "" || !reg.ReadsTranscriptFile() {
		return nil, errors.New("recovery supports top-level transcript files only; start a fresh parent session for subagent recovery")
	}
	semantics, err := sourceSemantics(opts.Sources, reg)
	if err != nil {
		return nil, err
	}
	if semantics.Mutation != agentapi.AppendOnly {
		return nil, errors.New("recovery requires an append-only native transcript")
	}
	adapter, err := sourceAdapter(opts.Sources, reg.Harness.Name)
	if err != nil {
		return nil, err
	}
	reader, available := newSourceReader(reg, opts)
	if !available {
		return nil, errors.New("current native transcript is unavailable")
	}
	filtered, _, err := reader.Filter(ctx, adapter, opts.maxTranscriptBytes())
	if err != nil {
		return nil, fmt.Errorf("filter current native transcript: %w", err)
	}
	// Recovery may replace a compacted transcript, but cannot reassign evidence
	// from another native session. An authoritative retained metadata identity
	// wins; copied/resumed records may contain several ownership IDs provided the
	// admitted session is among them. Missing identity facts retain hook authority.
	if filtered.LocalIdentity.ID != "" && filtered.LocalIdentity.ID != reg.NativeSessionID || len(filtered.SessionIDs) > 0 && !slices.Contains(filtered.SessionIDs, reg.NativeSessionID) {
		return nil, errors.New("current transcript identity differs from the registered session; recovery cannot attach another native session")
	}
	// Verify the candidate before presenting confirmation; no journal or archive ID
	// is allocated for empty/unsafe input or a publication excluded by skill policy.
	preview, err := archive.NewSourceBundle(reg, adapter, filtered, at, nil)
	if err != nil {
		return nil, err
	}
	if err := archive.CheckHistoryMutation(preview, archive.Metadata{}); err != nil {
		return nil, err
	}
	if opts.RequireSkillUse {
		rendered, err := renderPublication(ctx, opts.parserFor(reg.Harness.Name), opts.parserVersionFor(reg.Harness.Name), preview, reg, at, opts, func() string { return reg.RepoKey })
		if err != nil {
			return nil, err
		}
		if rendered.declined {
			return nil, errors.New("current transcript does not meet configured skill-use policy")
		}
	}
	if len(preview.NativeRecords) == 0 && len(preview.NativeText) == 0 {
		return nil, errors.New("current transcript retains no evidence")
	}
	return func(latest archive.SessionRegistration, id string) (archive.SessionRegistration, state.PendingPublication, error) {
		if latest.NativeSessionID != reg.NativeSessionID || latest.ProjectRoot != reg.ProjectRoot || latest.TranscriptPath != reg.TranscriptPath || latest.DestinationID != reg.DestinationID {
			return latest, state.PendingPublication{}, errors.New("registration changed during recovery; preview again")
		}
		latest.AdmissionStage = ""
		latest.ArchiveSessionID = id
		latest.PreviousGenerationID = reg.ArchiveSessionID
		bundle, err := archive.NewSourceBundle(latest, adapter, filtered, at, nil)
		if err != nil {
			return latest, state.PendingPublication{}, err
		}
		rendered, err := renderPublication(ctx, opts.parserFor(latest.Harness.Name), opts.parserVersionFor(latest.Harness.Name), bundle, latest, at, opts, func() string { return latest.RepoKey })
		if err != nil {
			return latest, state.PendingPublication{}, err
		}
		if rendered.declined {
			return latest, state.PendingPublication{}, errors.New("current transcript does not meet configured skill-use policy")
		}
		pending := state.PendingPublication{SkillEvidence: string(opts.skillEvidence()), Bundle: bundle, SourceKey: rendered.source.Key, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataKey: rendered.metadataKey, MetadataBytes: rendered.metadata, ReadyAt: at, Attempted: true}
		return latest, pending, nil
	}, nil
}

// maintainFrozen runs only from retained evidence. Privacy and parser upgrades
// may republish it with its original capture age; no native source or current
// skill inventory is observed, even if the live transcript grew or vanished.
func (s *sessionScan) maintainFrozen() (sessionOutcome, error) {
	if err := s.local.FrozenGeneration(s.reg); err != nil {
		return outcomeSkipped, err
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, err
	}
	if pending, found, err := s.local.LoadPending(s.id()); err != nil {
		return outcomeSkipped, err
	} else if found {
		if s.pendingPrivacyChanged(pending) {
			if _, err := s.maintainPendingPrivacy(pending); err != nil {
				return outcomeSkipped, err
			}
		} else if _, err := s.publishPending(pending); err != nil {
			return outcomeSkipped, err
		}
	}
	bundle, _, found := s.published.LastPublished()
	if !found {
		return outcomeSkipped, errors.New("frozen generation has no retained publication")
	}
	var retained []archive.SupplementalEvidence
	for _, evidence := range s.req.HookEvidence {
		if evidence.Kind == archive.EvidenceKindLinkedSession || evidence.Kind == archive.EvidenceKindExplicitFeedback {
			retained = append(retained, evidence)
		}
	}
	updated := mergeSupplementalEvidence(bundle.SupplementalEvidence, retained)
	sameLinks, err := jsonEncodingsEqual(bundle.SupplementalEvidence, updated)
	if err != nil {
		return outcomeSkipped, err
	}
	if !sameLinks || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
		if bundle.History != nil {
			return s.maintainCommittedPrivacy()
		}
		bundle.SupplementalEvidence = updated
		bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, s.opts.skillEvidence())
		filtered, err := refilterBundle(s.ctx, s.reg, adapter, bundle)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("refilter frozen retained history: %w", err)
		}
		// Never fall back to live input when retained refilter fails.
		maintenanceOptions := s.opts
		maintenanceOptions.RequireSkillUse = false // Existing history still requires current privacy maintenance.
		rendered, err := renderPublication(s.ctx, s.resolveParser(), s.parserVersion(), filtered, s.reg, s.now, maintenanceOptions, s.priorRepoKey)
		if err != nil {
			return outcomeSkipped, err
		}
		if rendered.declined {
			return outcomeSkipped, errors.New("frozen history maintenance cannot decline an existing publication")
		}
		pending := state.PendingPublication{SkillEvidence: string(s.opts.skillEvidence()), Bundle: filtered, SourceKey: rendered.source.Key, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataKey: rendered.metadataKey, MetadataBytes: rendered.metadata, ReadyAt: s.now, Attempted: true}
		if err := s.savePending(&pending); err != nil {
			return outcomeSkipped, err
		}
		outcome, err := s.publishPending(pending)
		if err != nil {
			return outcome, err
		}
		return outcome, s.completeRequest("complete frozen generation request")
	}
	outcome, handled, err := regenerateMetadata(s)
	if err != nil || handled {
		return outcome, err
	}
	return outcomeSkipped, s.completeRequest("complete frozen generation request")
}

// Frozen maintenance is settled by derivation and privacy policy, never by a
// stat of the live file. Validate bounded lineage authority before trusting
// its token; requests and publication/scan journals still force maintenance.
func (p *pass) unchangedFrozenSinceLastScan(reg archive.SessionRegistration) (bool, state.ScanSignature, error) {
	var signature state.ScanSignature
	if err := p.local.FrozenGeneration(reg); err != nil {
		return false, signature, err
	}
	if owed, err := p.linkOwed(reg); err != nil || owed {
		return false, signature, err
	}
	signature, found, err := p.local.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil || !found || !signature.Frozen || signature.Failed {
		return false, signature, err
	}
	adapterVersion, known := harnessAdapterVersion(p.opts.Sources, reg.Harness.Name)
	if !known || signature.ParserVersion != p.opts.parserVersionFor(reg.Harness.Name) || signature.FilterVersion != archive.FilterVersion || signature.AdapterVersion != adapterVersion || pendingSkillMode(signature.SkillEvidence) != p.opts.skillEvidence() || signature.PublishedLastHead != headFingerprint(reg.LastHead) {
		return false, signature, nil
	}
	unchanged, _, err := p.owesNothing(reg.ArchiveSessionID, false)
	return unchanged, signature, err
}

func (s *sessionScan) recordFrozenSignature() error {
	_, requested, err := s.local.LoadRequest(s.id())
	if err != nil {
		return err
	}
	// This scan is finishing; its own journal does not prevent recording the
	// signature, but queued evidence, uploads and rate-limited work still do.
	if outstanding, err := s.local.Outstanding(s.reg, requested); err != nil || outstanding.OwedAfterScan() {
		return err
	}
	adapterVersion, known := harnessAdapterVersion(s.opts.Sources, s.reg.Harness.Name)
	if !known {
		return nil
	}
	return s.local.SaveScanSignature(s.id(), state.ScanSignature{
		Frozen: true, SkillEvidence: string(s.opts.skillEvidence()),
		ParserVersion: s.parserVersion(), FilterVersion: archive.FilterVersion,
		AdapterVersion: adapterVersion, PublishedLastHead: s.publishedLastHead(),
	})
}
