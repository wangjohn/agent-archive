package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// persistPrivacy uses the shared capacity gate. Output is bounded in memory;
// no scratch payload is allocated, so the handle reserves no fictional disk bytes.
func (s *sessionScan) persistPrivacy(p state.PendingPublication) (err error) {
	if err = s.ctx.Err(); err != nil {
		return err
	}
	reservation, err := state.NewTemporaryReservation(s.local, state.PublicationPrivacy, s.id())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reservation.Close(), reservation.Err()) }()
	return s.local.SavePendingWithTemporaryReservation(reservation, s.id(), p)
}

func (s *sessionScan) maintainPendingPrivacy(original state.PendingPublication) (sessionOutcome, error) {
	prior, kind, err := s.reconcilePrivacyPending(original)
	if err != nil {
		return outcomeSkipped, err
	}
	if kind == state.PrivacyPending && original.Commit.Privacy != nil && original.Commit.Privacy.Authority == state.PrivacyPending {
		root, err := s.oldestPrivacyInput(original)
		if err != nil {
			return outcomeSkipped, err
		}
		prior, err = prior.CheckPrivacyReplayInput(original, *root, s.reg.DestinationID, s.publicationAdmission())
		if err != nil {
			return outcomeSkipped, err
		}
	}
	loader, err := s.pendingRetainedLoader(original)
	if err != nil {
		return outcomeSkipped, err
	}
	stageDigest, stageSHA := "", ""
	live, err := s.unreleasedPrivacyStage(original)
	if err != nil {
		return outcomeSkipped, err
	}
	if live {
		manifest, _, err := s.local.ReadAdmissionStage(s.id(), original.AdmissionStage)
		if err != nil {
			return outcomeSkipped, err
		}
		if err := state.CheckAdmissionStageOwnership(s.reg, manifest); err != nil {
			return outcomeSkipped, err
		}
		stageDigest, stageSHA = original.AdmissionStage, manifest.SHA256
		if kind == state.PrivacyPending && prior.State == state.PredecessorAbsent && original.SourceSHA256 == manifest.SHA256 {
			kind = state.PrivacyStage
			prior.PrivacyPendingSource = nil
		}
	}
	next, err := s.prepareRetainedPrivacy(original.MetadataBytes, loader, prior, kind, stageDigest, stageSHA, original.SkillEvidence)
	if err != nil {
		return outcomeSkipped, err
	}
	next.RequestToken = original.RequestToken
	if err := s.preservePrivacyInput(original, &next); err != nil {
		return outcomeSkipped, err
	}
	if err := s.persistPrivacy(next); err != nil {
		return outcomeSkipped, err
	}
	return s.publishPending(next)
}

func (s *sessionScan) maintainStagedPrivacy(manifest state.AdmissionStage, bundle archive.SourceBundle, req state.Request) (sessionOutcome, error) {
	prior := s.published.PublicationPredecessor()
	if prior.State == state.PredecessorPresent {
		return outcomeSkipped, errors.New("staged privacy requires exact committed/pending selection reconciliation")
	}
	key, err := archive.SourceObjectKey(bundle, manifest.SHA256)
	if err != nil {
		return outcomeSkipped, err
	}
	ref := archive.SourceReference{Key: key, SHA256: manifest.SHA256, CompressedBytes: int(manifest.Bytes)}
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, archive.Analysis{}, nil, s.opts.MachineID, s.reg.SessionStartedAt, s.now, ref, archive.ParserInfo{Version: s.parserVersion()})
	if err != nil {
		return outcomeSkipped, err
	}
	metadata.ApplyRegistrationProvenance(s.reg)
	metadata.ApplyProjectName(s.reg.ProjectRoot)
	metadata.ApplyRepoKey(s.reg.RepoKey)
	metadata.ApplyGitHead(s.reg)
	metadata.ApplyReplay(s.reg)
	raw, err := json.Marshal(metadata)
	if err != nil {
		return outcomeSkipped, err
	}
	loader := func(ctx context.Context, selected archive.RevisionReference) (archive.SourceBundle, error) {
		if err := ctx.Err(); err != nil {
			return archive.SourceBundle{}, err
		}
		if selected.Source != ref || !selected.CapturedAt.Equal(bundle.Capture.CapturedAt) {
			return archive.SourceBundle{}, state.ErrAdmissionStageRecovery
		}
		return bundle, nil
	}
	next, err := s.prepareRetainedPrivacy(raw, loader, prior, state.PrivacyStage, s.reg.AdmissionStage, manifest.SHA256, manifest.SkillEvidence)
	if err != nil {
		return outcomeSkipped, err
	}
	next.RequestToken = req.StageToken
	if err := s.persistPrivacy(next); err != nil {
		return outcomeSkipped, err
	}
	return s.publishPending(next)
}

// maintainCommittedPrivacy prepares all retained references before any replacement.
func (s *sessionScan) maintainCommittedPrivacy() (sessionOutcome, error) {
	prior := s.published.PublicationPredecessor()
	if prior.State != state.PredecessorPresent {
		return outcomeSkipped, errors.New("retained privacy maintenance needs exact complete predecessor authority")
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(prior.Body, &metadata); err != nil {
		return outcomeSkipped, err
	}
	next, err := s.prepareRetainedPrivacy(prior.Body, remoteRetainedLoader(s.remote, metadata), prior, state.PrivacyCommitted, "", "", "")
	if err != nil {
		return outcomeSkipped, fmt.Errorf("refilter complete retained selection: %w", err)
	}
	if err := s.persistPrivacy(next); err != nil {
		return outcomeSkipped, err
	}
	return s.publishPending(next)
}
