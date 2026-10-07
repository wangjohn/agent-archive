package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// MaxPublicationReplayBytes caps inline compressed replay bytes, not remote references.
const MaxPublicationReplayBytes = 128 << 20

// PublicationPurpose identifies the mutation authorized by a pending journal.
type PublicationPurpose string

const (
	// PublicationCapture publishes a validated selected source snapshot.
	PublicationCapture PublicationPurpose = "capture"
	// PublicationMetadata refreshes metadata over unchanged retained sources.
	PublicationMetadata PublicationPurpose = "metadata"
	// PublicationPrivacyRewrite replaces every retained source under current privacy policy.
	PublicationPrivacyRewrite PublicationPurpose = "privacy"
)

// PredecessorState distinguishes absent metadata from unavailable evidence.
type PredecessorState string

const (
	// PredecessorAbsent authorizes the first publication only when remote metadata is absent.
	PredecessorAbsent PredecessorState = "absent"
	// PredecessorPresent authorizes replacement of an exact recorded body.
	PredecessorPresent PredecessorState = "present"
	// PredecessorUnknown authorizes only completion of the exact next body already remote.
	PredecessorUnknown PredecessorState = "unknown"
)

// PublicationPredecessor is evidence from retained local committed state.
// Body is required only for Present; it must never be guessed from a remote read.
type PublicationPredecessor struct {
	State                  PredecessorState
	Body                   []byte
	Bundle                 archive.SourceBundle
	SameRevisionContinuity *PublicationContinuity
	Privacy                *PublicationPrivacyEvidence
	privacyReplaySource    *PendingPublication
	privacyReplay          *privacyReplayValidation
	PrivacyPendingSource   *PendingPublication
	RetainedPrivacy        *PublicationPrivacyEvidence
	PolicyContext          string
}

// PublicationContinuity binds a provider-approved continuation to exact source digests.
// Filtered equality alone cannot supply this evidence.
type PublicationContinuity struct {
	PreviousSourceSHA256 string `json:"PreviousSourceSHA256"`
	NextSourceSHA256     string `json:"NextSourceSHA256"`
}

// PublicationSource carries one exact source reference and optional replay bytes.
// The active source uses legacy SourceBytes instead, avoiding a duplicate payload.
// A future admitted stage port must resolve one bounded checksum/size-bound object
// at a time and release only after local commit and covered request completion.
type PublicationSource struct {
	Reference archive.SourceReference `json:"reference"`
	Bytes     []byte                  `json:"bytes,omitempty"`
}

// PublicationCommit binds replay to the complete source set and admitted context.
type PublicationCommit struct {
	Retention         *RetentionRestoration       `json:"retention,omitempty"`
	Privacy           *PublicationPrivacyEvidence `json:"privacy,omitempty"`
	Continuity        *PublicationContinuity      `json:"continuity,omitempty"`
	Version           int                         `json:"version"`
	MetadataSHA256    string                      `json:"metadata_sha256"`
	SourceSetSHA256   string                      `json:"source_set_sha256"`
	Predecessor       PredecessorState            `json:"predecessor"`
	PredecessorSHA256 string                      `json:"predecessor_sha256,omitempty"`
	DestinationID     string                      `json:"destination_id,omitempty"`
	AdmissionContext  string                      `json:"admission_context,omitempty"`
	PolicyContext     string                      `json:"policy_context"`
	Purpose           PublicationPurpose          `json:"purpose"`
}

func publicationSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PreparePublication seals exact bytes before remote writes. Ref-only entries
// need remote verification; absence of inline bytes never permits regeneration.
func PreparePublication(p PendingPublication, prior PublicationPredecessor, destination, admission, policy string, purpose PublicationPurpose) (PendingPublication, error) {
	if p.Commit != nil {
		return p, p.ValidatePublication()
	}
	if purpose != PublicationCapture && purpose != PublicationMetadata && purpose != PublicationPrivacyRewrite {
		return p, errors.New("unsupported publication purpose")
	}
	if err := p.validatePublicationPredecessor(prior, destination, admission, policy, purpose); err != nil {
		return p, err
	}
	digest, refs, err := archive.PublicationIdentity(p.MetadataBytes, destination, admission, policy, string(purpose))
	if err != nil {
		return p, err
	}
	provided := map[string]PublicationSource{}
	for _, source := range p.Sources {
		if _, exists := provided[source.Reference.Key]; exists {
			return p, errors.New("duplicate pending source payload")
		}
		provided[source.Reference.Key] = source
	}
	p.Sources = nil
	for _, ref := range refs {
		source := PublicationSource{Reference: ref}
		if payload, ok := provided[ref.Key]; ok {
			if payload.Reference != ref {
				return p, errors.New("pending payload reference differs from metadata")
			}
			source = payload
			delete(provided, ref.Key)
		}
		p.Sources = append(p.Sources, source)
	}
	if len(provided) != 0 {
		return p, errors.New("pending payload is not selected by metadata")
	}
	p.Commit = &PublicationCommit{Privacy: prior.Privacy, Continuity: prior.SameRevisionContinuity, Version: 1, MetadataSHA256: publicationSHA256(p.MetadataBytes), SourceSetSHA256: digest, Predecessor: prior.State, DestinationID: destination, AdmissionContext: admission, PolicyContext: policy, Purpose: purpose}
	if prior.State == PredecessorPresent {
		p.Commit.PredecessorSHA256 = publicationSHA256(prior.Body)
	}
	return p, p.ValidatePublication()
}

func (p PendingPublication) validatePublicationPredecessor(prior PublicationPredecessor, destination, admission, policy string, purpose PublicationPurpose) error {
	switch prior.State {
	case PredecessorPresent:
		if len(prior.Body) == 0 {
			return errors.New("publication predecessor body is unavailable")
		}
		var previous, next archive.Metadata
		if err := json.Unmarshal(prior.Body, &previous); err != nil {
			return err
		}
		if err := json.Unmarshal(p.MetadataBytes, &next); err != nil {
			return err
		}
		if _, _, err := archive.PublicationIdentity(prior.Body, destination, admission, policy, string(purpose)); err != nil {
			return fmt.Errorf("invalid publication predecessor: %w", err)
		}
		if previous.SessionID != next.SessionID || previous.NativeSessionID != next.NativeSessionID || previous.ProjectID != next.ProjectID || previous.MachineID != next.MachineID {
			return errors.New("publication cannot change committed ownership")
		}
		if purpose == PublicationPrivacyRewrite {
			return validatePrivacyPreparation(prior, previous, next, destination, admission)
		}
		if previous.History != nil && next.History != nil && previous.History.CurrentRevision == next.History.CurrentRevision && previous.SourceBundle != next.SourceBundle {
			proof := prior.SameRevisionContinuity
			if proof == nil || proof.PreviousSourceSHA256 != previous.SourceBundle.SHA256 || proof.NextSourceSHA256 != next.SourceBundle.SHA256 {
				return errors.New("same revision update requires provider-approved continuity; retry selection validation")
			}
		}
		if err := archive.ValidateRevisionTransition(previous, next, prior.Bundle, p.Bundle); err != nil {
			return err
		}
	case PredecessorAbsent, PredecessorUnknown:
		return p.validateUncommittedPrivacy(prior, destination, admission, purpose)

	default:
		return errors.New("publication predecessor evidence is required")
	}
	return nil
}

// ValidatePublication checks every local replay payload and exact metadata binding.
func (p PendingPublication) ValidatePublication() error {
	c := p.Commit
	if c == nil || c.Version != 1 || (c.Purpose != PublicationCapture && c.Purpose != PublicationMetadata && c.Purpose != PublicationPrivacyRewrite) {
		return errors.New("pending source-set journal is incomplete")
	}
	if c.Predecessor != PredecessorAbsent && c.Predecessor != PredecessorPresent && c.Predecessor != PredecessorUnknown {
		return errors.New("invalid publication predecessor state")
	}
	if (c.Predecessor == PredecessorPresent && !validPublicationDigest(c.PredecessorSHA256)) || (c.Predecessor != PredecessorPresent && c.PredecessorSHA256 != "") {
		return errors.New("invalid publication predecessor digest")
	}
	digest, refs, err := archive.PublicationIdentity(p.MetadataBytes, c.DestinationID, c.AdmissionContext, c.PolicyContext, string(c.Purpose))
	if err != nil {
		return err
	}
	if digest != c.SourceSetSHA256 || publicationSHA256(p.MetadataBytes) != c.MetadataSHA256 || len(refs) != len(p.Sources) || len(refs) > archive.MaxPreservedRevisions+1 {
		return errors.New("pending publication identity mismatch")
	}
	if err := p.validatePrivacyEvidence(); err != nil {
		return err
	}
	if err := p.validateRetentionProof(); err != nil {
		return err
	}
	if c.Continuity != nil && (c.Predecessor != PredecessorPresent || !validPublicationDigest(c.Continuity.PreviousSourceSHA256) || c.Continuity.NextSourceSHA256 != p.SourceSHA256) {
		return errors.New("publication continuation binding differs")
	}
	if err := p.validatePublicationOwnership(refs[0]); err != nil {
		return err
	}
	return p.validatePublicationPayloads(refs)
}

func (p PendingPublication) validatePublicationOwnership(active archive.SourceReference) error {
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return err
	}
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil || p.MetadataKey != key || active != p.SourceReference() || p.Bundle.ArchiveSessionID != m.SessionID || p.Bundle.NativeSessionID != m.NativeSessionID || p.Bundle.ProjectID != m.ProjectID || p.Bundle.Capture.Harness != m.Harness || !p.Bundle.Capture.CapturedAt.Equal(m.CapturedAt) {
		return errors.New("pending publication source or ownership mismatch")
	}
	if p.Bundle.Capture.FilterVersion != m.FilterVersion || p.Bundle.Capture.AdapterName != m.Adapter.Name || p.Bundle.Capture.AdapterVersion != m.Adapter.Version {
		return errors.New("pending source filter provenance differs from metadata")
	}
	if (p.Bundle.History == nil) != (m.History == nil) {
		return errors.New("pending source selection differs from metadata")
	}
	if m.History != nil {
		if err := p.Bundle.ValidateHistory(); err != nil {
			return err
		}
		if p.Bundle.History.ActiveRolloutID != m.History.CurrentRevision {
			return errors.New("pending active revision differs from metadata")
		}
	}
	return nil
}

func (p PendingPublication) validatePublicationPayloads(refs []archive.SourceReference) error {
	total := len(p.SourceBytes)
	for i, source := range p.Sources {
		if source.Reference != refs[i] || source.Reference.CompressedBytes > 128<<20 {
			return errors.New("pending publication source set differs from metadata")
		}
		payload := source.Bytes
		if i == 0 {
			if len(payload) > 0 {
				return errors.New("active replay bytes must use the legacy payload field")
			}
			payload = p.SourceBytes
		} else {
			total += len(payload)
		}
		if len(payload) > 0 && (len(payload) != source.Reference.CompressedBytes || publicationSHA256(payload) != source.Reference.SHA256) {
			return errors.New("pending source checksum or size does not match persisted bytes")
		}
	}
	if total > MaxPublicationReplayBytes {
		return errors.New("pending replay exceeds 128 MiB; publish bounded groups without dropping retained references")
	}
	return nil
}

func validPublicationDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

// SaveCommittedPublication atomically records the complete remotely verified
// set and exact metadata before any request acknowledgement or stage release.
func (p *Published) SaveCommittedPublication(pending PendingPublication, at time.Time) error {
	if pending.Commit == nil {
		return errors.New("committed publication requires a sealed source set")
	}
	if pending.MetadataOnly && p.found {
		return p.SaveRepublishedMetadata(pending, at)
	}
	return p.SavePublication(pending.Bundle, at, pending.SourceReference(), pending.MetadataBytes, pending)

}

// CommittedSources returns every exact retained reference of the last publication.
// Legacy state uses cached metadata, or its single recorded source when available.
func (p *Published) CommittedSources() ([]archive.SourceReference, error) {
	if p.state.Commit != nil {
		digest, refs, err := archive.PublicationIdentity(p.state.MetadataBytes, p.state.Commit.DestinationID, p.state.Commit.AdmissionContext, p.state.Commit.PolicyContext, string(p.state.Commit.Purpose))
		if err != nil {
			return nil, err
		}
		if digest != p.state.Commit.SourceSetSHA256 || publicationSHA256(p.state.MetadataBytes) != p.state.Commit.MetadataSHA256 || !slices.Equal(refs, p.state.Sources) {
			return nil, errors.New("committed publication identity is damaged; reconcile retained state")
		}
		return refs, nil
	}
	if len(p.state.MetadataBytes) > 0 {
		var m archive.Metadata
		if err := json.Unmarshal(p.state.MetadataBytes, &m); err != nil {
			return nil, err
		}
		return m.SourceReferences()
	}
	if ref, found := p.LastPublishedSource(); found {
		return []archive.SourceReference{ref}, nil
	}
	return nil, nil
}

// PublicationPredecessor returns replacement authority from durable local state.
// A remote-only legacy cache migration remains unknown even after restart.
func (p *Published) PublicationPredecessor() PublicationPredecessor {
	bundle, _, published := p.LastPublished()
	if !published {
		return PublicationPredecessor{State: PredecessorAbsent}
	}
	if p.state.PredecessorUnknown || len(p.state.MetadataBytes) == 0 || (p.state.Commit != nil && publicationSHA256(p.state.MetadataBytes) != p.state.Commit.MetadataSHA256) {
		return PublicationPredecessor{State: PredecessorUnknown}
	}
	policy := ""
	if p.state.Commit != nil {
		policy = p.state.Commit.PolicyContext
	}
	var retained *PublicationPrivacyEvidence
	if p.state.Commit != nil {
		evidence := p.state.Commit.Privacy
		if evidence != nil && len(evidence.Sources) > 0 {
			pending := PendingPublication{Commit: p.state.Commit, MetadataBytes: p.state.MetadataBytes, AdmissionStage: evidence.StageDigest, SkillEvidence: evidence.Sources[0].NewPolicy.Skill}
			_, sourceErr := p.CommittedSources()
			if sourceErr == nil && pending.validatePrivacyEvidence() == nil {
				retained = evidence
			}
		}
	}
	return PublicationPredecessor{State: PredecessorPresent, Body: p.state.MetadataBytes, Bundle: bundle, PolicyContext: policy, RetainedPrivacy: retained}
}

func attachPublication(next publishedState, pending PendingPublication) (publishedState, error) {
	if err := pending.ValidatePublication(); err != nil {
		return next, err
	}
	commit := *pending.Commit
	next.Commit = &commit
	next.PredecessorUnknown = false
	next.Sources = make([]archive.SourceReference, len(pending.Sources))
	for i, source := range pending.Sources {
		next.Sources[i] = source.Reference
	}
	return next, nil
}

func (p PendingPublication) validateUncommittedPrivacy(prior PublicationPredecessor, destination, admission string, purpose PublicationPurpose) error {
	if purpose == PublicationPrivacyRewrite && prior.Privacy != nil && prior.Privacy.Authority == PrivacyPending {
		if err := validatePendingPrivacyPreparation(prior, destination, admission); err != nil {
			return err
		}
	}
	if purpose == PublicationPrivacyRewrite && (prior.Privacy == nil || (prior.Privacy.Authority != PrivacyStage && prior.Privacy.Authority != PrivacyPending) || prior.Privacy.Authority == PrivacyStage && prior.Privacy.StageDigest != p.AdmissionStage) {
		return errors.New("privacy replacement requires committed predecessor or immutable admitted stage authority")
	}
	if len(prior.Body) != 0 {
		return errors.New("unexpected publication predecessor body")
	}
	return nil
}

func (p PendingPublication) validateRetentionProof() error {
	c := p.Commit
	if c.Retention != nil && (c.Predecessor != PredecessorAbsent || c.Purpose != PublicationCapture || !validPublicationDigest(c.Retention.DeletionSHA256) || !validPublicationDigest(c.Retention.OwnerSHA256) || c.Retention.ReplacementSHA256 != c.MetadataSHA256 || !validPublicationDigest(c.Retention.PredecessorSHA256) || c.Retention.CoveredToken == "" || c.Retention.CoveredToken != p.RequestToken) {
		return errors.New("invalid retention restoration authority")
	}
	return nil
}
