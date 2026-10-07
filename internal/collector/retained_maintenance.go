package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// retainedSourceLoader resolves one admitted/committed filtered selection at a time.
type retainedSourceLoader func(context.Context, archive.RevisionReference) (archive.SourceBundle, error)

func remoteRetainedLoader(remote storage.ObjectStore, metadata archive.Metadata) retainedSourceLoader {
	return func(ctx context.Context, ref archive.RevisionReference) (archive.SourceBundle, error) {
		return reader.LoadRevisionSource(ctx, remote, metadata, ref, reader.Limits{MaxCompressedBytes: 128 << 20, MaxUncompressedBytes: 128 << 20})
	}
}

// preserveRetainedMetadata keeps immutable creation, ownership and revision tuples
// when analysis rebuilds only the currently selected filtered source.
func preserveRetainedMetadata(next *archive.Metadata, prior archive.Metadata) {
	next.SessionID = prior.SessionID
	next.NativeSessionID = prior.NativeSessionID
	next.ProjectID = prior.ProjectID
	next.MachineID = prior.MachineID
	next.StartedAt = prior.StartedAt
	next.ParentSessionID = prior.ParentSessionID
	next.PreviousGenerationID = prior.PreviousGenerationID
	next.Origin = prior.Origin
	next.ImportedAt = prior.ImportedAt
	next.StartedAtSource = prior.StartedAtSource
	next.Replay = prior.Replay
	next.ProjectName = prior.ProjectName
	next.RepoKey = prior.RepoKey
	next.GitHead = prior.GitHead
	if prior.History != nil {
		h := *prior.History
		h.Preserved = append([]archive.RevisionReference(nil), prior.History.Preserved...)
		next.History = &h
	}
}

func retainedPolicy(b archive.SourceBundle, skill string) state.PublicationPolicy {
	return state.PublicationPolicy{Filter: b.Capture.FilterVersion, Adapter: b.Capture.AdapterName, Version: b.Capture.AdapterVersion, Format: b.Capture.SourceFormat, Skill: skill, SkillAuthority: state.PrivacySkillConfigured}
}

// prepareRetainedPrivacy invokes the real native filter on every selected source.
// It has no native file/Git fallback and produces no remote writes. Callers hold
// the shared atomic pending quota gate before persisting the bounded RAM output.
func (s *sessionScan) prepareRetainedPrivacy(raw []byte, loader retainedSourceLoader, prior state.PublicationPredecessor, authority state.PrivacyAuthority, stageDigest, stageSHA, oldSkill string) (state.PendingPublication, error) {
	var before archive.Metadata
	if err := json.Unmarshal(raw, &before); err != nil {
		return state.PendingPublication{}, err
	}
	if _, _, err := archive.PublicationIdentity(raw, s.reg.DestinationID, s.publicationAdmission(), "", string(state.PublicationPrivacyRewrite)); err != nil {
		return state.PendingPublication{}, err
	}
	selections, err := state.RevisionSelections(before)
	if err != nil {
		return state.PendingPublication{}, err
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return state.PendingPublication{}, err
	}
	next := before
	preserveRetainedMetadata(&next, before)
	proof := state.PublicationPrivacyEvidence{Authority: authority, StageDigest: stageDigest, StageSourceSHA256: stageSHA, PreviousPolicyContext: storage.SHA256Hex([]byte(before.FilterVersion + "\x00" + before.Adapter.Version + "\x00" + oldSkill))}
	if prior.PolicyContext != "" {
		proof.PreviousPolicyContext = prior.PolicyContext
	}
	if authority == state.PrivacyPending {
		original := prior.PrivacyPendingSource
		if original == nil || original.Commit == nil {
			return state.PendingPublication{}, errors.New("sealed pending privacy authority is unavailable")
		}
		c := original.Commit
		proof.PreviousPolicyContext = c.PolicyContext
		proof.PendingMutation = &state.PrivacyPendingMutation{MetadataBytes: original.MetadataBytes, MetadataSHA256: c.MetadataSHA256, SourceSetSHA256: c.SourceSetSHA256, PolicyContext: c.PolicyContext, Purpose: c.Purpose, Predecessor: c.Predecessor, PredecessorSHA256: c.PredecessorSHA256, Continuity: c.Continuity}
	}
	pending := state.PendingPublication{AdmissionStage: stageDigest, SkillEvidence: string(s.opts.skillEvidence()), MetadataKey: s.mustMetadataKey(), RequestToken: s.req.Token, ReadyAt: s.now, Attempted: true}
	total := 0
	for i, selected := range selections {
		if err := s.ctx.Err(); err != nil {
			return pending, err
		}
		bundle, err := loader(s.ctx, selected)
		if err != nil {
			return pending, fmt.Errorf("load retained revision %q: %w", selected.RevisionID, err)
		}
		reg := s.reg
		reg.Harness = bundle.Capture.Harness
		reg.PreviousGenerationID = bundle.PreviousGenerationID
		reg.ParentSessionID = bundle.ParentSessionID
		original := bundle
		bundle.SupplementalEvidence = limitSkillEvidence(bundle.SupplementalEvidence, s.opts.skillEvidence())
		if i == 0 {
			bundle.SupplementalEvidence = mergeSupplementalEvidence(bundle.SupplementalEvidence, retainedMaintenanceEvidence(s.req.HookEvidence))
		}
		filtered, err := refilterRetainedBundle(s.ctx, reg, adapter, bundle)
		if err != nil {
			return pending, fmt.Errorf("refilter retained revision %q: %w", selected.RevisionID, err)
		}
		if err := validateRetainedPrivacyFacts(original, filtered); err != nil {
			return pending, err
		}
		compressed, err := archive.BuildCompressedSource(filtered)
		if err != nil {
			return pending, err
		}
		if len(compressed.Bytes) > state.MaxPublicationReplayBytes-total {
			return pending, errors.New("complete privacy replacement exceeds inline replay capacity; retain evidence and retry with capacity")
		}
		total += len(compressed.Bytes)
		key, err := archive.SourceObjectKey(filtered, compressed.SHA256)
		if err != nil {
			return pending, err
		}
		ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
		replacement := selected
		replacement.Source = ref
		oldPolicy := retainedPolicy(original, oldSkill)
		if oldSkill == "" || i > 0 {
			oldPolicy.SkillAuthority = state.PrivacySkillObserved
			oldPolicy.Skill = observedRetainedSkill(original.SupplementalEvidence)
		}
		proof.Sources = append(proof.Sources, state.PrivacySource{Previous: selected, Next: replacement, OldPolicy: oldPolicy, NewPolicy: retainedPolicy(filtered, string(s.opts.skillEvidence()))})
		if i == 0 {
			pending.Bundle = filtered
			pending.SourceKey = key
			pending.SourceSHA256 = ref.SHA256
			pending.SourceBytes = compressed.Bytes
			built, err := s.deriveRetainedPrivacyMetadata(filtered, ref, before)
			if err != nil {
				return pending, err
			}

			next = built
		} else {
			next.History.Preserved[i-1] = replacement
			pending.Sources = append(pending.Sources, state.PublicationSource{Reference: ref, Bytes: compressed.Bytes})
		}
	}
	pending.MetadataBytes, err = json.Marshal(next)
	if err != nil {
		return pending, err
	}
	policy, err := s.publicationPolicy(pending.Bundle)
	if err != nil {
		return pending, err
	}
	prior, err = s.bindRetainedPrivacyEvidence(proof, raw, pending.MetadataBytes, prior, policy)
	if err != nil {
		return pending, err
	}

	return state.PreparePublication(pending, prior, s.reg.DestinationID, s.publicationAdmission(), policy, state.PublicationPrivacyRewrite)
}

func (s *sessionScan) mustMetadataKey() string {
	key, _ := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	return key
}

func retainedMaintenanceEvidence(in []archive.SupplementalEvidence) []archive.SupplementalEvidence {
	var out []archive.SupplementalEvidence
	for _, e := range in {
		if e.Kind == archive.EvidenceKindLinkedSession || e.Kind == archive.EvidenceKindExplicitFeedback {
			out = append(out, e)
		}
	}
	return out
}

func validateRetainedPrivacyFacts(prior, next archive.SourceBundle) error {
	if prior.ArchiveSessionID != next.ArchiveSessionID || prior.NativeSessionID != next.NativeSessionID || prior.ProjectID != next.ProjectID || prior.Capture.Harness != next.Capture.Harness || prior.ParentSessionID != next.ParentSessionID || prior.PreviousGenerationID != next.PreviousGenerationID || !prior.Capture.CapturedAt.Equal(next.Capture.CapturedAt) || (prior.History == nil) != (next.History == nil) {
		return errors.New("native privacy refilter changed retained ownership, age or history")
	}
	if prior.History == nil {
		return nil
	}
	before, after := *prior.History, *next.History
	before.Spans = append([]archive.HistorySpan(nil), before.Spans...)
	after.Spans = append([]archive.HistorySpan(nil), after.Spans...)
	for i := range before.Spans {
		before.Spans[i].FirstRecord = 0
		before.Spans[i].EndRecord = 0
	}
	for i := range after.Spans {
		after.Spans[i].FirstRecord = 0
		after.Spans[i].EndRecord = 0
	}
	if !reflect.DeepEqual(before, after) {
		return errors.New("native privacy refilter changed physical history facts")
	}
	return next.ValidateHistory()
}

func (s *sessionScan) verifyRetainedSelection(metadata archive.Metadata) error {
	refs, err := state.RevisionSelections(metadata)
	if err != nil {
		return err
	}
	loader := remoteRetainedLoader(s.remote, metadata)
	for _, ref := range refs {
		if _, err := loader(s.ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

func observedRetainedSkill(evidence []archive.SupplementalEvidence) string {
	mode := config.SkillEvidenceNone
	for _, item := range evidence {
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			return string(config.SkillEvidenceBody)
		}
		if item.Kind == archive.EvidenceKindSkillInventory {
			mode = config.SkillEvidenceMetadata
		}
	}
	return string(mode)
}

func (s *sessionScan) deriveRetainedPrivacyMetadata(filtered archive.SourceBundle, ref archive.SourceReference, before archive.Metadata) (archive.Metadata, error) {
	analysis, parseErr := agentapi.Analyze(s.ctx, s.resolveParser(), filtered)
	if errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded) {
		return archive.Metadata{}, parseErr
	}
	built, buildErr := archive.BuildMetadataWithAnalysis(filtered, analysis, parseErr, before.MachineID, before.StartedAt, s.now, ref, archive.ParserInfo{Version: s.parserVersion()})
	if buildErr != nil && !archive.IsParseError(buildErr) {
		return archive.Metadata{}, buildErr
	}
	preserveRetainedMetadata(&built, before)
	built.ApplyGitHead(s.reg)
	return built, nil
}

func (s *sessionScan) bindRetainedPrivacyEvidence(proof state.PublicationPrivacyEvidence, raw, next []byte, prior state.PublicationPredecessor, policy string) (state.PublicationPredecessor, error) {
	stageDigest := proof.StageDigest
	var err error
	if stageDigest != "" {
		retained := prior.RetainedPrivacy
		if prior.PrivacyPendingSource != nil && prior.PrivacyPendingSource.Commit != nil {
			retained = prior.PrivacyPendingSource.Commit.Privacy
		}
		if retained != nil {
			if err := state.ComposeStagePrivacy(&proof, retained); err != nil {
				return prior, err
			}
		}
	}
	prior.Privacy, err = state.BindPrivacyEvidence(proof, raw, next, s.reg.DestinationID, s.publicationAdmission(), policy)
	if err != nil {
		return prior, err
	}
	return prior, nil
}
