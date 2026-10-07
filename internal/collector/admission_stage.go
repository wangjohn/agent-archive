package collector

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// stagedAdmission runs before pending/native selection. Corruption or changed
// privacy stays pending; no raw native evidence substitutes for admitted bytes.
func (s *sessionScan) stagedAdmission() (sessionOutcome, bool, error) {
	if s.reg.AdmissionStage == "" {
		return outcomeSkipped, false, nil
	}
	released, err := s.local.AdmissionStageReleased(s.reg)
	if err != nil {
		return outcomeSkipped, true, err
	}
	if released {
		return outcomeSkipped, false, nil
	}
	if resumed, err := s.local.ResumeAdmissionStageRelease(s.reg, s.published); resumed || err != nil {
		return outcomeSkipped, true, err
	}
	manifest, bundle, err := s.local.ReadAdmissionStage(s.id(), s.reg.AdmissionStage)
	if err != nil {
		return outcomeSkipped, true, err
	}
	if err = state.CheckAdmissionStageOwnership(s.reg, manifest); err != nil {
		return outcomeSkipped, true, err
	}
	if err = s.local.ReconcileAdmissionStage(s.reg, manifest, s.opts.AcceptSession); err != nil {
		return outcomeSkipped, true, err
	}
	req, _, err := s.local.LoadRequest(s.id())
	if err != nil {
		return outcomeSkipped, true, err
	}
	if (req.StageDigest != "" && req.StageDigest != s.reg.AdmissionStage) || (req.StageToken != "" && req.StageDigest != s.reg.AdmissionStage) {
		return outcomeSkipped, true, state.ErrAdmissionStageRecovery
	}
	pending, found, err := s.local.LoadPending(s.id())
	if err != nil {
		return outcomeSkipped, true, err
	}
	if found {
		outcome, err := s.resumeStagedPending(pending, manifest, bundle)
		return outcome, true, err
	}

	if err := s.requireNoOrphanPrivacyInput(); err != nil {
		return outcomeSkipped, true, err
	}

	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, true, err
	}
	if bundle.Capture.FilterVersion != archive.FilterVersion || bundle.Capture.AdapterVersion != adapter.Version() || manifest.SkillEvidence != string(s.opts.skillEvidence()) || !sourceEvidenceWithinPolicy(bundle.SupplementalEvidence, s.opts.skillEvidence()) {
		outcome, err := s.maintainStagedPrivacy(manifest, bundle, req)
		return outcome, true, err
	}
	// A crash after local commit/request completion but before release does not
	// replay the old stage indefinitely or acknowledge a newer unrelated request.
	if s.local.AdmissionStageCommitted(s.reg, manifest, s.published) {
		err = s.local.ReleaseAdmissionStage(s.reg, manifest, s.published, req.StageToken)
		return outcomeSkipped, true, err
	}
	rendered, err := renderPublication(s.ctx, s.resolveParser(), s.parserVersion(), bundle, s.reg, s.now, s.opts, s.priorRepoKey)
	if err != nil {
		return outcomeSkipped, true, err
	}
	if rendered.declined {
		return outcomeSkipped, true, errors.New("durable import remains pending under current skill policy")
	}
	if rendered.source.SHA256 != manifest.SHA256 {
		return outcomeSkipped, true, state.ErrAdmissionStageRecovery
	}
	pending = state.PendingPublication{AdmissionStage: s.reg.AdmissionStage, SkillEvidence: string(s.opts.skillEvidence()), Bundle: bundle, SourceKey: rendered.source.Key, MetadataKey: rendered.metadataKey, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataBytes: rendered.metadata, RequestToken: req.StageToken, ReadyAt: s.now, Attempted: true}
	if err = s.savePending(&pending); err != nil {
		return outcomeSkipped, true, err
	}
	outcome, err := s.publishPending(pending)
	return outcome, true, err
}

func (s *sessionScan) resumeStagedPending(pending state.PendingPublication, manifest state.AdmissionStage, bundle archive.SourceBundle) (sessionOutcome, error) {
	if s.pendingPrivacyChanged(pending) {
		if pending.AdmissionStage != s.reg.AdmissionStage {
			return outcomeSkipped, state.ErrAdmissionStageRecovery
		}
		outcome, err := s.maintainPendingPrivacy(pending)
		return outcome, err
	}
	if pending.AdmissionStage != s.reg.AdmissionStage {
		return outcomeSkipped, state.ErrAdmissionStageRecovery
	}
	if pending.SourceSHA256 != manifest.SHA256 {
		if err := pending.CheckAdmissionStageTransform(s.reg, manifest, bundle); err != nil {
			return outcomeSkipped, err
		}
	}

	outcome, err := s.publishPending(pending)
	return outcome, err
}
