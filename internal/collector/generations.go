package collector

import (
	"context"
	"errors"
	"fmt"
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
	// Verify the candidate before presenting confirmation; no journal or archive ID
	// is allocated for empty/unsafe input or a publication excluded by skill policy.
	preview, err := archive.NewSourceBundle(reg, adapter, filtered, at, nil)
	if err != nil {
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
		if pending.Bundle.Capture.FilterVersion != archive.FilterVersion || pending.Bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(pending.Bundle.SupplementalEvidence, s.opts.skillEvidence()) {
			// Stronger privacy supersedes a retained-history maintenance retry.
			// Rebuild below from the last acknowledged publication, without native
			// input and without carrying the discarded retry's newer age forward.
			if err := s.local.RemovePending(s.id()); err != nil {
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
	var links []archive.SupplementalEvidence
	for _, evidence := range s.req.HookEvidence {
		if evidence.Kind == archive.EvidenceKindLinkedSession {
			links = append(links, evidence)
		}
	}
	updated := mergeSupplementalEvidence(bundle.SupplementalEvidence, links)
	sameLinks, err := jsonEncodingsEqual(bundle.SupplementalEvidence, updated)
	if err != nil {
		return outcomeSkipped, err
	}
	if !sameLinks || bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
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
		if err := s.local.SavePending(s.id(), pending); err != nil {
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
