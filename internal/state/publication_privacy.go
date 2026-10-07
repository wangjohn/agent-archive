package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// PrivacyAuthority distinguishes committed selection from unuploaded admitted evidence.
type PrivacyAuthority string

const (
	// PrivacyCommitted is an exact locally committed predecessor body.
	PrivacyCommitted PrivacyAuthority = "committed"
	// PrivacyStage is immutable admitted filtered evidence, never a remote predecessor.
	PrivacyStage PrivacyAuthority = "stage"
	// PrivacyPending is an exact previously sealed authorized candidate.
	PrivacyPending PrivacyAuthority = "pending"
)

// PublicationPolicy records each source's native privacy codec and skill policy.
type PublicationPolicy struct {
	Filter  string `json:"filter"`
	Adapter string `json:"adapter"`
	Version string `json:"version"`
	Format  string `json:"format"`
	Skill   string `json:"skill"`
}

// PrivacySource binds one injected-codec transformation to an unchanged selection.
type PrivacySource struct {
	Previous  archive.RevisionReference `json:"previous"`
	Next      archive.RevisionReference `json:"next"`
	OldPolicy PublicationPolicy         `json:"old_policy"`
	NewPolicy PublicationPolicy         `json:"new_policy"`
}

// PublicationPrivacyEvidence seals the complete correspondence produced by the
// injected retained filter. It is not append continuity or raw dependency proof.
// PrivacyPendingMutation retains the original sealed mutation authority without payload duplication.
type PrivacyPendingMutation struct {
	MetadataSHA256    string                 `json:"metadata_sha256"`
	SourceSetSHA256   string                 `json:"source_set_sha256"`
	PolicyContext     string                 `json:"policy_context"`
	Purpose           PublicationPurpose     `json:"purpose"`
	Predecessor       PredecessorState       `json:"predecessor"`
	PredecessorSHA256 string                 `json:"predecessor_sha256,omitempty"`
	Continuity        *PublicationContinuity `json:"continuity,omitempty"`
}

type PublicationPrivacyEvidence struct {
	PendingMutation        *PrivacyPendingMutation `json:"pending_mutation,omitempty"`
	Authority              PrivacyAuthority        `json:"authority"`
	StageDigest            string                  `json:"stage_digest,omitempty"`
	StageSourceSHA256      string                  `json:"stage_source_sha256,omitempty"`
	PreviousMetadataSHA256 string                  `json:"previous_metadata_sha256"`
	PreviousSetSHA256      string                  `json:"previous_set_sha256"`
	PreviousPolicyContext  string                  `json:"previous_policy_context"`
	NextMetadataSHA256     string                  `json:"next_metadata_sha256"`
	NextSetSHA256          string                  `json:"next_set_sha256"`
	OwnershipSHA256        string                  `json:"ownership_sha256"`
	Sources                []PrivacySource         `json:"sources"`
}

// RevisionSelections returns current first, then the exact preserved revision tuples.
func RevisionSelections(m archive.Metadata) ([]archive.RevisionReference, error) {
	if _, err := m.SourceReferences(); err != nil {
		return nil, err
	}
	current := archive.RevisionReference{Source: m.SourceBundle, CapturedAt: m.CapturedAt}
	if m.History == nil {
		return []archive.RevisionReference{current}, nil
	}
	current.RevisionID = m.History.CurrentRevision
	return append([]archive.RevisionReference{current}, m.History.Preserved...), nil
}

func privacyOwnership(m archive.Metadata) string {
	raw, _ := json.Marshal(struct {
		Session       string
		Native        string
		Project       string
		Machine       string
		Harness       string
		Parent        string
		Generation    string
		Started       time.Time
		Origin        archive.SessionOrigin
		Imported      *time.Time
		StartedSource archive.StartedAtSource
	}{Session: m.SessionID, Native: m.NativeSessionID, Project: m.ProjectID, Machine: m.MachineID, Harness: m.Harness.Name, Parent: m.ParentSessionID, Generation: m.PreviousGenerationID, Started: m.StartedAt, Origin: m.Origin, Imported: m.ImportedAt, StartedSource: m.StartedAtSource})
	return publicationSHA256(raw)
}

// BindPrivacyEvidence completes codec-produced correspondence with exact metadata identities.
// The collector must supply only transformations it produced via its injected native filter.
func BindPrivacyEvidence(e PublicationPrivacyEvidence, previous, next []byte, destination, admission, policy string) (*PublicationPrivacyEvidence, error) {
	var before, after archive.Metadata
	if err := json.Unmarshal(previous, &before); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(next, &after); err != nil {
		return nil, err
	}
	if privacyOwnership(before) != privacyOwnership(after) {
		return nil, errors.New("privacy replacement changes retained ownership or creation")
	}
	var err error
	e.PreviousSetSHA256, _, err = archive.PublicationIdentity(previous, destination, admission, e.PreviousPolicyContext, string(PublicationPrivacyRewrite))
	if err != nil {
		return nil, err
	}
	e.NextSetSHA256, _, err = archive.PublicationIdentity(next, destination, admission, policy, string(PublicationPrivacyRewrite))
	if err != nil {
		return nil, err
	}
	e.PreviousMetadataSHA256 = publicationSHA256(previous)
	e.NextMetadataSHA256 = publicationSHA256(next)
	e.OwnershipSHA256 = privacyOwnership(after)
	old, err := RevisionSelections(before)
	if err != nil {
		return nil, err
	}
	if len(old) != len(e.Sources) {
		return nil, errors.New("privacy transform does not account for every previous reference")
	}
	for i, source := range e.Sources {
		if source.Previous != old[i] {
			return nil, errors.New("privacy transform previous selection differs")
		}
	}
	return &e, nil
}

func validatePrivacyPreparation(p PendingPublication, prior PublicationPredecessor, previous, next archive.Metadata, destination, admission, policy string) error {
	e := prior.Privacy
	if e != nil && e.Authority == PrivacyPending {
		return validatePendingPrivacyPreparation(prior, destination, admission, policy)
	}
	if e == nil || e.Authority != PrivacyCommitted || e.PreviousMetadataSHA256 != publicationSHA256(prior.Body) || e.OwnershipSHA256 != privacyOwnership(previous) || privacyOwnership(previous) != privacyOwnership(next) {
		return errors.New("privacy replacement requires exact complete committed authority")
	}
	digest, _, err := archive.PublicationIdentity(prior.Body, destination, admission, e.PreviousPolicyContext, string(PublicationPrivacyRewrite))
	if err != nil || digest != e.PreviousSetSHA256 {
		return errors.New("privacy predecessor source-set identity differs")
	}
	old, err := RevisionSelections(previous)
	if err != nil {
		return err
	}
	if len(old) != len(e.Sources) {
		return errors.New("privacy replacement cannot omit a retained reference")
	}
	for i, source := range e.Sources {
		if source.Previous != old[i] {
			return errors.New("privacy previous correspondence differs")
		}
	}
	return nil
}

func (p PendingPublication) validatePrivacyEvidence() error {
	c := p.Commit
	if c.Purpose != PublicationPrivacyRewrite {
		if c.Privacy != nil {
			return errors.New("privacy proof cannot authorize a different mutation purpose")
		}
		return nil
	}
	e := c.Privacy
	if e == nil || !validPublicationDigest(e.PreviousMetadataSHA256) || !validPublicationDigest(e.PreviousSetSHA256) || e.NextMetadataSHA256 != c.MetadataSHA256 || e.NextSetSHA256 != c.SourceSetSHA256 {
		return errors.New("privacy publication correspondence is incomplete")
	}
	switch e.Authority {
	case PrivacyCommitted:
		if c.Predecessor != PredecessorPresent || e.PreviousMetadataSHA256 != c.PredecessorSHA256 {
			return errors.New("committed privacy authority is not the exact predecessor")
		}
	case PrivacyStage:
		if c.Predecessor == PredecessorPresent || e.StageDigest == "" || e.StageDigest != p.AdmissionStage || !validPublicationDigest(e.StageDigest) || !validPublicationDigest(e.StageSourceSHA256) {
			return errors.New("stage privacy authority cannot impersonate a committed predecessor")
		}
	case PrivacyPending:
		original := e.PendingMutation
		if original == nil || original.MetadataSHA256 != e.PreviousMetadataSHA256 || !validPublicationDigest(original.SourceSetSHA256) || original.Predecessor != c.Predecessor || original.PredecessorSHA256 != c.PredecessorSHA256 || original.Predecessor == PredecessorUnknown || (original.Purpose != PublicationCapture && original.Purpose != PublicationMetadata && original.Purpose != PublicationPrivacyRewrite) {
			return errors.New("pending privacy input is not exact known mutation authority")
		}
	default:
		return errors.New("privacy input authority is unknown")
	}
	if e.Authority != PrivacyPending && e.PendingMutation != nil {
		return errors.New("pending authority cannot authorize another privacy input kind")
	}
	if e.StageDigest != "" && (e.StageDigest != p.AdmissionStage || !validPublicationDigest(e.StageDigest) || !validPublicationDigest(e.StageSourceSHA256)) {
		return errors.New("privacy stage receipt differs from immutable admission")
	}
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		return err
	}
	next, err := RevisionSelections(m)
	if err != nil {
		return err
	}
	if privacyOwnership(m) != e.OwnershipSHA256 || len(e.Sources) != len(next) {
		return errors.New("privacy transform changes ownership or reference count")
	}
	seen := map[string]bool{}
	stageBound := false
	for i, source := range e.Sources {
		if source.Next != next[i] || source.Previous.RevisionID != source.Next.RevisionID || !source.Previous.CapturedAt.Equal(source.Next.CapturedAt) || seen[source.Previous.Source.Key] || source.Previous.Source.CompressedBytes <= 0 || !validPublicationDigest(source.Previous.Source.SHA256) {
			return errors.New("privacy correspondence changes selection, age or uniqueness")
		}
		expected := fmt.Sprintf("sessions/%s/%s/source.%s.jsonl.gz", m.Harness.Name, m.SessionID, source.Previous.Source.SHA256)
		if source.Previous.Source.Key != expected {
			return errors.New("privacy previous source belongs to another namespace")
		}
		seen[source.Previous.Source.Key] = true
		if source.NewPolicy.Filter != m.FilterVersion || source.NewPolicy.Adapter != m.Adapter.Name || source.NewPolicy.Version != m.Adapter.Version || source.NewPolicy.Skill != p.SkillEvidence || source.NewPolicy.Format == "" || source.OldPolicy.Filter == "" || source.OldPolicy.Adapter == "" || source.OldPolicy.Version == "" || source.OldPolicy.Format == "" {
			return errors.New("privacy transform codec policy differs")
		}
		if source.Previous.Source.SHA256 == e.StageSourceSHA256 {
			stageBound = true
		}
	}
	if e.StageDigest != "" && !stageBound {
		return errors.New("privacy transform does not bind admitted source")
	}
	return nil
}

// PrivacyRetiredSources returns the complete old-key intent without duplicating selected sources.
func (p PendingPublication) PrivacyRetiredSources() []archive.SourceReference {
	if p.Commit == nil || p.Commit.Purpose != PublicationPrivacyRewrite || p.Commit.Privacy == nil {
		return nil
	}
	var out []archive.SourceReference
	for _, source := range p.Commit.Privacy.Sources {
		if source.Previous.Source != source.Next.Source && !slices.Contains(out, source.Previous.Source) {
			out = append(out, source.Previous.Source)
		}
	}
	return out
}

// privacyStageSource authenticates transformed stage membership from the selecting commit.
func (p *Published) privacyStageSource(reg archive.SessionRegistration, m AdmissionStage) (string, bool) {
	if p.state.Commit == nil || p.state.Commit.Privacy == nil {
		return "", false
	}
	e := p.state.Commit.Privacy
	if e.StageDigest != reg.AdmissionStage || e.StageSourceSHA256 != m.SHA256 || len(e.Sources) == 0 {
		return "", false
	}
	pending := PendingPublication{Commit: p.state.Commit, MetadataBytes: p.state.MetadataBytes, AdmissionStage: reg.AdmissionStage, SkillEvidence: e.Sources[0].NewPolicy.Skill}
	if pending.validatePrivacyEvidence() != nil {
		return "", false
	}
	refs, err := p.CommittedSources()
	if err != nil {
		return "", false
	}
	for _, source := range e.Sources {
		if source.Previous.Source.SHA256 == m.SHA256 && int64(source.Previous.Source.CompressedBytes) == m.Bytes && slices.Contains(refs, source.Next.Source) {
			return source.Next.Source.SHA256, true
		}
	}
	return "", false
}

func validatePendingPrivacyPreparation(prior PublicationPredecessor, destination, admission, policy string) error {
	e, original := prior.Privacy, prior.PrivacyPendingSource
	if original == nil || original.Commit == nil || e.PendingMutation == nil || original.ValidatePublication() != nil || original.Commit.Predecessor == PredecessorUnknown || original.Commit.DestinationID != destination || original.Commit.AdmissionContext != admission || original.Commit.MetadataSHA256 != e.PreviousMetadataSHA256 || original.Commit.SourceSetSHA256 != e.PendingMutation.SourceSetSHA256 || original.Commit.PolicyContext != e.PendingMutation.PolicyContext || original.Commit.Purpose != e.PendingMutation.Purpose || original.Commit.Predecessor != e.PendingMutation.Predecessor || original.Commit.PredecessorSHA256 != e.PendingMutation.PredecessorSHA256 {
		return errors.New("privacy pending input differs from original sealed mutation")
	}
	check := prior
	check.Privacy = nil
	check.PrivacyPendingSource = nil
	check.SameRevisionContinuity = original.Commit.Continuity
	if original.Commit.Predecessor != check.State || check.State == PredecessorPresent && publicationSHA256(check.Body) != original.Commit.PredecessorSHA256 {
		return errors.New("privacy pending predecessor differs from exact retained authority")
	}
	if err := original.validatePublicationPredecessor(check, destination, admission, original.Commit.PolicyContext, original.Commit.Purpose); err != nil {
		return err
	}
	var before archive.Metadata
	if err := json.Unmarshal(original.MetadataBytes, &before); err != nil {
		return err
	}
	selections, err := RevisionSelections(before)
	if err != nil {
		return err
	}
	if len(selections) != len(e.Sources) {
		return errors.New("privacy pending correspondence omits candidate revisions")
	}
	for i, source := range e.Sources {
		if source.Previous != selections[i] {
			return errors.New("privacy input is not the exact pending selection")
		}
	}
	digest, _, err := archive.PublicationIdentity(original.MetadataBytes, destination, admission, e.PreviousPolicyContext, string(PublicationPrivacyRewrite))
	if err != nil || digest != e.PreviousSetSHA256 {
		return errors.New("privacy pending selection identity differs")
	}
	return nil
}
