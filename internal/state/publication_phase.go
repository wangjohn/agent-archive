package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// PreparationAuthority freezes the exact original selection and inputs.
// Progress and retired cleanup are deliberately outside its digest.
type PreparationAuthority struct {
	PrivacyPreviousMetadata []byte                    `json:"privacy_previous_metadata,omitempty"`
	Migration               *OrdinaryHistoryMigration `json:"migration,omitempty"`
	Version                 int                       `json:"version"`
	Kind                    string                    `json:"kind"`
	SessionID               string                    `json:"session_id"`
	OwnerSHA256             string                    `json:"owner_sha256"`
	DestinationID           string                    `json:"destination_id"`
	AdmissionContext        string                    `json:"admission_context"`
	PolicyContext           string                    `json:"policy_context"`
	Purpose                 PublicationPurpose        `json:"purpose"`
	Predecessor             PredecessorState          `json:"predecessor"`
	PredecessorSHA256       string                    `json:"predecessor_sha256,omitempty"`
	OriginMetadata          []byte                    `json:"origin_metadata"`
	OriginMetadataSHA256    string                    `json:"origin_metadata_sha256"`
	OriginSetSHA256         string                    `json:"origin_set_sha256"`
	Inputs                  []PreparationInput        `json:"inputs"`
	OriginalEvidenceSHA256  string                    `json:"original_evidence_sha256,omitempty"`
	SHA256                  string                    `json:"sha256"`
}

// PreparationInput is one immutable input in the original ordered selection.
type PreparationInput struct {
	HookObservations *PublicationHookObservations `json:"hook_observations,omitempty"`
	Reference        archive.SourceReference      `json:"reference"`
	Selection        PublicationSelection         `json:"selection"`
	FilterVersion    string                       `json:"filter_version"`
	AdapterVersion   string                       `json:"adapter_version"`
	SkillPolicy      string                       `json:"skill_policy"`
	Payload          PublicationPayload           `json:"payload"`
}

// PreparedOutput binds a completed sequential transform to its original index.
type PreparedOutput struct {
	Privacy    *PrivacySource    `json:"privacy,omitempty"`
	InputIndex int               `json:"input_index"`
	Source     PublicationSource `json:"source"`
}

// PreparationProgress is the independently checksummed mutable checkpoint.
type PreparationProgress struct {
	Version         int              `json:"version"`
	AuthoritySHA256 string           `json:"authority_sha256"`
	Cursor          int              `json:"cursor"`
	Outputs         []PreparedOutput `json:"outputs"`
	WorkingMetadata []byte           `json:"working_metadata"`
	SHA256          string           `json:"sha256"`
}

// CleanupProgress keeps retirement outside immutable selecting authority.
type CleanupProgress struct {
	Version                 int             `json:"version"`
	SelectingMetadataSHA256 string          `json:"selecting_metadata_sha256"`
	SelectingSetSHA256      string          `json:"selecting_set_sha256"`
	Retired                 []RetiredSource `json:"retired"`
	SHA256                  string          `json:"sha256"`
}

func preparationSHA(a PreparationAuthority) string {
	type inputBinding struct {
		Hook                                                      *PublicationHookFacts
		Reference                                                 archive.SourceReference
		Selection                                                 PublicationSelection
		FilterVersion, AdapterVersion, SkillPolicy, PayloadSHA256 string
	}
	inputs := make([]inputBinding, len(a.Inputs))
	for i, input := range a.Inputs {
		inputs[i] = inputBinding{publicationHookFacts(input.HookObservations), input.Reference, input.Selection, input.FilterVersion, input.AdapterVersion, input.SkillPolicy, payloadSetSHA([]PublicationSource{{Reference: input.Reference, Selection: input.Selection, Payload: input.Payload}})}
	}
	raw, _ := json.Marshal(struct {
		Version                                                                          int
		Kind, SessionID, OwnerSHA256, DestinationID, AdmissionContext, PolicyContext     string
		Purpose                                                                          PublicationPurpose
		Predecessor                                                                      PredecessorState
		PredecessorSHA256, OriginMetadataSHA256, OriginSetSHA256, OriginalEvidenceSHA256 string
		Inputs                                                                           []inputBinding
	}{a.Version, a.Kind, a.SessionID, a.OwnerSHA256, a.DestinationID, a.AdmissionContext, a.PolicyContext, a.Purpose, a.Predecessor, a.PredecessorSHA256, a.OriginMetadataSHA256, a.OriginSetSHA256, a.OriginalEvidenceSHA256, inputs})
	previousPrivacySHA := publicationSHA256(a.PrivacyPreviousMetadata)
	migration := ""
	if a.Migration != nil {
		migration = a.Migration.SHA256
	}
	return publicationSHA256(append(append([]byte("preparation-authority/v1\x00"), raw...), []byte("\x00"+migration+"\x00"+previousPrivacySHA)...))
}
func progressSHA(p PreparationProgress) string {
	type outputBinding struct {
		Index         int
		PayloadSHA256 string
		PrivacySHA256 string
	}
	outputs := make([]outputBinding, len(p.Outputs))
	for i, o := range p.Outputs {
		proofSHA := ""
		if o.Privacy != nil {
			proofSHA = o.Privacy.SHA256
		}
		outputs[i] = outputBinding{o.InputIndex, payloadSetSHA([]PublicationSource{o.Source}), proofSHA}
	}
	raw, _ := json.Marshal(struct {
		Version               int
		AuthoritySHA256       string
		Cursor                int
		Outputs               []outputBinding
		WorkingMetadataSHA256 string
	}{p.Version, p.AuthoritySHA256, p.Cursor, outputs, publicationSHA256(p.WorkingMetadata)})
	return publicationSHA256(append([]byte("preparation-progress/v1\x00"), raw...))
}
func cleanupSHA(c CleanupProgress) string {
	c.SHA256 = ""
	raw, _ := json.Marshal(c)
	return publicationSHA256(append([]byte("cleanup-progress/v1\x00"), raw...))
}

// PreparePublicationV2 freezes a preparing adjunct or seals a ready transaction.
// It grants no storage capability; the guarded writer persists composition8 first.
func PreparePublicationV2(p PendingPublication, prior PublicationPredecessor, destination, admission, policy string, purpose PublicationPurpose) (PendingPublication, error) {
	preparing := p.History != nil && p.History.Preparing
	if p.JournalVersion == 2 && p.Commit != nil {
		if err := p.ValidatePublication(); err != nil {
			return p, fmt.Errorf("existing selecting seal: %w", err)
		}
		return p, nil
	}
	if p.JournalVersion != 0 && p.JournalVersion != 2 {
		return p, ErrDurableStorageRecovery
	}
	p.Commit = nil
	sources, err := selectedPublicationSources(p, destination, admission)
	if err != nil {
		return p, err
	}
	p.Sources = sources
	var m archive.Metadata
	if err = json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return p, err
	}
	if p.Preparation == nil {
		digest, _, e := archive.PublicationIdentity(p.MetadataBytes, destination, admission, policy, string(purpose))
		if e != nil {
			return p, e
		}
		a := &PreparationAuthority{Version: 1, Kind: "capture", SessionID: m.SessionID, OwnerSHA256: publicationOwner(m, destination, admission), DestinationID: destination, AdmissionContext: admission, PolicyContext: policy, Purpose: purpose, Predecessor: prior.State, OriginMetadata: slices.Clone(p.MetadataBytes), OriginMetadataSHA256: publicationSHA256(p.MetadataBytes), OriginSetSHA256: digest}
		if purpose == PublicationPrivacyRewrite {
			a.Kind = "privacy-committed"
			if prior.State == PredecessorAbsent {
				a.Kind = "privacy-pending-absent"
			}
			if prior.State == PredecessorPresent && !bytes.Equal(prior.Body, p.MetadataBytes) {
				a.Kind = "privacy-pending"
				a.PrivacyPreviousMetadata = slices.Clone(prior.Body)
			}
		}
		if prior.State == PredecessorPresent {
			a.PredecessorSHA256 = publicationSHA256(prior.Body)
		}
		for _, source := range sources {
			input := PreparationInput{Reference: source.Reference, Selection: source.Selection, FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, SkillPolicy: p.SkillEvidence, Payload: source.Payload}
			if p.History != nil {
				for _, h := range p.History.Inputs {
					if h.Reference == input.Reference {
						input.FilterVersion = h.FilterVersion
						input.Selection.CapturedAt = h.CapturedAt.UTC()
						input.Selection.SourceSchemaVersion = h.SourceSchemaVersion
					}
				}
			}
			if input.Selection.Role == "current" {
				input.HookObservations = p.hookObservations
			}
			a.Inputs = append(a.Inputs, input)
		}
		if p.migration != nil {
			receipt := p.migration.receipt
			a.Migration = &receipt
		}
		a.SHA256 = preparationSHA(*a)
		p.Preparation = a
	}
	p.JournalVersion = 2
	p.Phase = "ready"
	if preparing {
		p.Phase = "preparing"
	}
	if p.History != nil {
		p.History.Version = 2
		if !preparing && p.Progress == nil {
			if len(p.History.Inputs) == 0 {
				for _, input := range p.Preparation.Inputs {
					p.History.Inputs = append(p.History.Inputs, HistoryInput{Reference: input.Reference, RevisionID: input.Selection.RevisionID, CapturedAt: input.Selection.CapturedAt, SourceSchemaVersion: input.Selection.SourceSchemaVersion, FilterVersion: input.FilterVersion})
				}
			}
			p.History.PrivacyCursor = len(p.Preparation.Inputs)
		}
		cursor := p.History.PrivacyCursor
		progress := &PreparationProgress{Version: 1, AuthoritySHA256: p.Preparation.SHA256, Cursor: cursor, WorkingMetadata: slices.Clone(p.MetadataBytes)}
		for i := 0; i < cursor; i++ {
			input := p.Preparation.Inputs[i]
			var output *PublicationSource
			for j := range sources {
				if sources[j].Selection.RevisionID == input.Selection.RevisionID {
					output = &sources[j]
					break
				}
			}
			if output == nil {
				return p, errors.New("completed preparation input has no selected output")
			}
			completed := PreparedOutput{InputIndex: i, Source: *output}
			if proof, ok := p.privacyOutputs[i]; ok {
				copy := proof
				completed.Privacy = &copy
			} else if p.Progress != nil && i < len(p.Progress.Outputs) {
				completed.Privacy = p.Progress.Outputs[i].Privacy
			}
			progress.Outputs = append(progress.Outputs, completed)
		}
		progress.SHA256 = progressSHA(*progress)
		p.Progress = progress
	}
	digest, _, err := archive.PublicationIdentity(p.MetadataBytes, destination, admission, policy, string(purpose))
	if err != nil {
		return p, err
	}
	c := &CleanupProgress{Version: 1, SelectingMetadataSHA256: publicationSHA256(p.MetadataBytes), SelectingSetSHA256: digest}
	if p.History != nil {
		c.Retired = slices.Clone(p.History.Retired)
	}
	c.SHA256 = cleanupSHA(*c)
	p.Cleanup = c
	if !preparing {
		if err = p.validatePublicationPredecessor(prior, destination, admission, policy, purpose); err != nil {
			return p, fmt.Errorf("selecting predecessor correspondence: %w", err)
		}
		p.Commit = &PublicationCommit{Version: 2, MetadataSHA256: publicationSHA256(p.MetadataBytes), SourceSetSHA256: digest, PayloadSetSHA256: payloadSetSHA(sources), PreparationSHA256: p.Preparation.SHA256, PrivacySHA256: privacyReceiptsSHA(p.privacyReceipts()), Predecessor: prior.State, DestinationID: destination, AdmissionContext: admission, PolicyContext: policy, Purpose: purpose}
		if prior.State == PredecessorPresent {
			p.Commit.PredecessorSHA256 = publicationSHA256(prior.Body)
		}
	}
	if err := p.validatePublicationEnvelope(); err != nil {
		return p, fmt.Errorf("new selecting preparation envelope: %w", err)
	}
	return p, nil
}

func (p PendingPublication) validatePublicationEnvelope() error {
	if p.JournalVersion != 2 || (p.Phase != "preparing" && p.Phase != "ready") || p.Preparation == nil || p.Cleanup == nil {
		return ErrDurableStorageRecovery
	}
	a := p.Preparation
	if p.Commit != nil && p.Commit.SettledPrivacySHA256 != "" {
		return ErrDurableStorageRecovery
	}
	if len(p.MetadataBytes) > 32<<20 || len(a.OriginMetadata) > 32<<20 {
		return ErrDurableStorageCapacity
	}
	if err := a.validate(); err != nil {
		return fmt.Errorf("frozen preparation authority: %w", err)
	}
	if a.Migration != nil && p.Phase == "ready" && a.Migration.NextMetadataSHA256 != publicationSHA256(p.MetadataBytes) {
		return ErrDurableStorageRecovery
	}
	var working archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &working); err != nil {
		return err
	}
	expected, err := selectedPublicationSources(p, a.DestinationID, a.AdmissionContext)
	if err != nil {
		return err
	}
	if len(expected) != len(p.Sources) {
		return ErrDurableStorageRecovery
	}
	inline, compressed := 0, 0
	for _, input := range a.Inputs {
		if h := input.HookObservations; h != nil {
			inline += len(h.Body)
			compressed += len(h.Body)
		}
	}
	for i, source := range p.Sources {
		if source.Reference != expected[i].Reference || source.Selection != expected[i].Selection || source.Reference.CompressedBytes <= 0 || source.Reference.CompressedBytes > 128<<20 || len(source.Bytes) != 0 {
			return ErrDurableStorageRecovery
		}
		if err := validatePublicationPayload(source, working, a.DestinationID, a.AdmissionContext); err != nil {
			return err
		}
		inline += len(source.Payload.Inline)
		if source.Reference.CompressedBytes > (128<<20)-compressed && p.History != nil {
			return ErrDurableStorageCapacity
		}
		compressed += source.Reference.CompressedBytes
	}
	if inline > 128<<20 {
		return ErrDurableStorageCapacity
	}
	if p.Phase == "preparing" {
		if p.Commit != nil || p.History == nil || !p.History.Preparing || p.Attempted {
			return ErrDurableStorageRecovery
		}
	} else if p.Commit == nil || p.Commit.Version != 2 || p.History != nil && p.History.Preparing {
		return fmt.Errorf("ready preparation commit: %w", ErrDurableStorageRecovery)
	}
	if p.History != nil {
		g := p.Progress
		if g == nil || g.Version != 1 || g.AuthoritySHA256 != a.SHA256 || g.SHA256 != progressSHA(*g) || g.Cursor != p.History.PrivacyCursor || len(g.Outputs) != g.Cursor || g.Cursor < 0 || g.Cursor > len(a.Inputs) || publicationSHA256(g.WorkingMetadata) != publicationSHA256(p.MetadataBytes) {
			return ErrDurableStorageRecovery
		}
		for i, output := range g.Outputs {
			if output.Privacy != nil {
				if err := output.Privacy.validate(*a, i, a.Inputs[i], output.Source); err != nil {
					return err
				}
			} else if output.Source.Reference != a.Inputs[i].Reference {
				return fmt.Errorf("changed frozen output without transformation receipt: %w", ErrDurableStorageRecovery)
			}
			if output.InputIndex != i || output.Source.Selection.RevisionID != a.Inputs[i].Selection.RevisionID {
				return ErrDurableStorageRecovery
			}
		}
	}
	c := p.Cleanup
	if c.Version != 1 || c.SHA256 != cleanupSHA(*c) || c.SelectingMetadataSHA256 != publicationSHA256(p.MetadataBytes) {
		return ErrDurableStorageRecovery
	}
	if p.History != nil && !slices.Equal(c.Retired, p.History.Retired) {
		return ErrDurableStorageRecovery
	}
	if p.Phase == "ready" {
		if p.Commit.PreparationSHA256 != a.SHA256 || p.Commit.PayloadSetSHA256 != payloadSetSHA(p.Sources) {
			return ErrDurableStorageRecovery
		}
		if p.Commit.PrivacySHA256 != privacyReceiptsSHA(p.privacyReceipts()) {
			return ErrDurableStorageRecovery
		}
		if p.Commit.Purpose == PublicationPrivacyRewrite {
			if err := validatePrivacyCorrespondence(*a, p.Sources, p.privacyReceipts(), p.MetadataBytes, privacyPreviousBody(*a)); err != nil {
				return err
			}
		}
		return p.validateReadyPublication()
	}
	return nil
}

// publicationWire clears compatibility byte fields; the union is the only wire payload.
func publicationWire(p PendingPublication) PendingPublication {
	if p.JournalVersion == 2 {
		p.SourceBytes = nil
		for i := range p.Sources {
			if len(p.Sources[i].Bytes) != 0 {
				p.Sources = slices.Clone(p.Sources)
				p.Sources[i].Bytes = nil
			}
		}
	}
	return p
}

// validate checks immutable preparation authority without a mutable cursor.
func (a PreparationAuthority) validate() error {
	if len(a.OriginMetadata) == 0 || len(a.OriginMetadata) > 32<<20 {
		return ErrDurableStorageCapacity
	}
	if a.Version != 1 || (a.Kind != "capture" && a.Kind != "privacy-committed" && a.Kind != "privacy-pending" && a.Kind != "privacy-pending-absent") || a.SHA256 != preparationSHA(a) || a.OriginMetadataSHA256 != publicationSHA256(a.OriginMetadata) || len(a.Inputs) == 0 || len(a.Inputs) > 65 {
		return ErrDurableStorageRecovery
	}
	for _, input := range a.Inputs {
		if h := input.HookObservations; h != nil {
			if validatePublicationHookFacts(&h.Facts, input, a.OwnerSHA256, a.DestinationID, a.AdmissionContext, a.PolicyContext) != nil || len(h.Body) != h.Facts.BodySize || publicationSHA256(h.Body) != h.Facts.BodySHA256 {
				return ErrDurableStorageRecovery
			}
		}
	}
	switch a.Kind {
	case "capture":
		if a.Purpose != PublicationCapture && a.Purpose != PublicationMetadata || len(a.PrivacyPreviousMetadata) != 0 {
			return ErrDurableStorageRecovery
		}
	case "privacy-pending-absent":
		if a.Purpose != PublicationPrivacyRewrite || a.Predecessor != PredecessorAbsent || a.PredecessorSHA256 != "" || len(a.PrivacyPreviousMetadata) != 0 || a.Migration != nil {
			return ErrDurableStorageRecovery
		}
	case "privacy-committed":
		if a.Purpose != PublicationPrivacyRewrite || a.Predecessor != PredecessorPresent || a.PredecessorSHA256 != a.OriginMetadataSHA256 || len(a.PrivacyPreviousMetadata) != 0 || a.Migration != nil {
			return ErrDurableStorageRecovery
		}
	case "privacy-pending":
		if a.Purpose != PublicationPrivacyRewrite || a.Predecessor != PredecessorPresent || len(a.PrivacyPreviousMetadata) == 0 || len(a.PrivacyPreviousMetadata) > 32<<20 || publicationSHA256(a.PrivacyPreviousMetadata) != a.PredecessorSHA256 || a.Migration != nil {
			return ErrDurableStorageRecovery
		}
	}
	if a.Migration != nil {
		r := a.Migration
		if len(r.PreviousMetadata) == 0 || len(r.PreviousMetadata) > 32<<20 {
			return ErrDurableStorageRecovery
		}
		var previous, next archive.Metadata
		if err := json.Unmarshal(r.PreviousMetadata, &previous); err != nil {
			return err
		}
		if err := json.Unmarshal(a.OriginMetadata, &next); err != nil {
			return err
		}
		if err := r.validate(previous, next, r.PreviousMetadata, a.OriginMetadata, a.DestinationID, a.AdmissionContext, a.PolicyContext); err != nil {
			return err
		}

	}
	var origin archive.Metadata
	if err := json.Unmarshal(a.OriginMetadata, &origin); err != nil {
		return err
	}
	if a.SessionID != origin.SessionID || a.OwnerSHA256 != publicationOwner(origin, a.DestinationID, a.AdmissionContext) {
		return ErrDurableStorageRecovery
	}
	digest, refs, err := archive.PublicationIdentity(a.OriginMetadata, a.DestinationID, a.AdmissionContext, a.PolicyContext, string(a.Purpose))
	if err != nil || digest != a.OriginSetSHA256 || len(refs) != len(a.Inputs) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	for i, input := range a.Inputs {
		if input.Reference != refs[i] {
			return ErrDurableStorageRecovery
		}
		if err := validatePublicationPayload(PublicationSource{Reference: input.Reference, Selection: input.Selection, Payload: input.Payload}, origin, a.DestinationID, a.AdmissionContext); err != nil {
			return err
		}
	}
	return nil
}
