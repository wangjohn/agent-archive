package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/reader"
	"os"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// privacyInputJournal preserves one oldest sealed input until an exact local successor.
// The latest transition remains in pending; neither journal is permission to upload old policy.
type privacyInputJournal struct {
	Checksum                          string                   `json:"checksum"`
	Version                           int                      `json:"version"`
	Session                           string                   `json:"session"`
	Original                          state.PendingPublication `json:"original"`
	ReplacementMetadataSHA256         string                   `json:"replacement_metadata_sha256"`
	ReplacementSetSHA256              string                   `json:"replacement_set_sha256"`
	PreviousJournalSHA256             string                   `json:"previous_journal_sha256,omitempty"`
	PreviousReplacementMetadataSHA256 string                   `json:"previous_replacement_metadata_sha256,omitempty"`
	PreviousReplacementSetSHA256      string                   `json:"previous_replacement_set_sha256,omitempty"`
	Destination                       string                   `json:"destination"`
	Admission                         string                   `json:"admission"`
}

func privacyJournalBytes(j privacyInputJournal) ([]byte, error) {
	j.Checksum = ""
	raw, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	j.Checksum = storage.SHA256Hex(raw)
	return json.Marshal(j)
}

func (s *sessionScan) readPrivacyInputJournal() (privacyInputJournal, []byte, error) {
	raw, err := s.local.ReadPublicationEvidence(s.id())
	if err != nil {
		return privacyInputJournal{}, nil, err
	}
	var j privacyInputJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return j, nil, fmt.Errorf("privacy original evidence requires recovery: %w", err)
	}
	expected, err := privacyJournalBytes(j)
	if err != nil || string(expected) != string(raw) || j.Version != 1 || j.Session != s.id() || j.Destination != s.reg.DestinationID || j.Admission != s.publicationAdmission() || j.Original.ValidatePublication() != nil || j.Original.Bundle.ArchiveSessionID != s.id() || j.Original.Bundle.NativeSessionID != s.reg.NativeSessionID || j.Original.Bundle.ProjectID != s.reg.ProjectID || j.Original.Bundle.Capture.Harness.Name != s.reg.Harness.Name || j.Original.Commit.Predecessor == state.PredecessorUnknown || j.Original.Commit.DestinationID != j.Destination || j.Original.Commit.AdmissionContext != j.Admission {
		return j, nil, errors.New("privacy original evidence seal or ownership requires recovery")
	}
	return j, raw, nil
}

func replayAuthority(p state.PendingPublication) *state.PrivacyPendingMutation {
	c := p.Commit
	var receipt *state.PublicationPrivacyEvidence
	if c.Privacy != nil {
		retained := *c.Privacy
		retained.ReplayInput = nil
		retained.InputJournalSHA256 = ""
		receipt = &retained
	}
	return &state.PrivacyPendingMutation{MetadataBytes: p.MetadataBytes, MetadataSHA256: c.MetadataSHA256, SourceSetSHA256: c.SourceSetSHA256, PolicyContext: c.PolicyContext, Purpose: c.Purpose, Predecessor: c.Predecessor, PredecessorSHA256: c.PredecessorSHA256, Continuity: c.Continuity, Privacy: receipt}
}

// originalSourcesRecoverable checks actual immutable evidence, never a checksum assertion.
func (s *sessionScan) originalSourcesRecoverable(p state.PendingPublication) (bool, error) {
	live, err := s.unreleasedPrivacyStage(p)
	if err != nil {
		return false, err
	}
	if live && len(p.Sources) == 1 {
		m, _, err := s.local.ReadAdmissionStage(s.id(), p.AdmissionStage)
		if err == nil && state.CheckAdmissionStageOwnership(s.reg, m) == nil && p.SourceSHA256 == m.SHA256 && int64(p.SourceReference().CompressedBytes) == m.Bytes {
			return true, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	refs := make([]storage.SourcePublication, len(p.Sources))
	for i, source := range p.Sources {
		refs[i] = storage.SourcePublication{Key: source.Reference.Key, SHA256: source.Reference.SHA256, Size: source.Reference.CompressedBytes}
	}
	err = storage.VerifySourceSet(s.ctx, s.remote, refs, s.opts.Retry)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// preservePrivacyInput must finish before SavePending replaces any original bytes.
func (s *sessionScan) preservePrivacyInput(original state.PendingPublication, next *state.PendingPublication) error {
	if original.ValidatePublication() != nil || next.ValidatePublication() != nil {
		return errors.New("privacy input and replacement must be completely sealed")
	}
	proof := next.Commit.Privacy
	proof.ReplayInput = replayAuthority(original)
	if proof.Authority == state.PrivacyPending && original.Commit.Privacy != nil && original.Commit.Privacy.Authority == state.PrivacyPending && original.Commit.Privacy.ReplayInput != nil {
		proof.ReplayInput = original.Commit.Privacy.ReplayInput
	}
	j, raw, err := s.readPrivacyInputJournal()
	found := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if original.Commit.Privacy != nil && original.Commit.Privacy.InputJournalSHA256 != "" {
		if !found && !s.exactLocalPrivacySuccessor(original) || found && !privacyJournalMatches(j, raw, original) {
			return errors.New("privacy original evidence journal is missing or differs; retain pending")
		}
	}
	recoverable, err := s.originalSourcesRecoverable(original)
	if err != nil {
		return err
	}
	if found && (original.Commit.Privacy == nil || original.Commit.Privacy.InputJournalSHA256 == "") && j.Original.Commit.MetadataSHA256 != original.Commit.MetadataSHA256 {
		return errors.New("unsettled original publication evidence differs from pending authority")
	}
	if !found && recoverable {
		return next.ValidatePublication()
	}
	if !found {
		j = privacyInputJournal{Version: 1, Session: s.id(), Original: original, Destination: s.reg.DestinationID, Admission: s.publicationAdmission()}
	}
	if found {
		j.PreviousJournalSHA256 = storage.SHA256Hex(raw)
		if original.Commit.Privacy != nil && original.Commit.Privacy.InputJournalSHA256 != "" {
			j.PreviousJournalSHA256 = original.Commit.Privacy.InputJournalSHA256
		}
		j.PreviousReplacementMetadataSHA256 = original.Commit.MetadataSHA256
		j.PreviousReplacementSetSHA256 = original.Commit.SourceSetSHA256
	}
	j.ReplacementMetadataSHA256 = next.Commit.MetadataSHA256
	j.ReplacementSetSHA256 = next.Commit.SourceSetSHA256
	encoded, err := privacyJournalBytes(j)
	if err != nil {
		return err
	}
	if err := s.local.SavePublicationEvidence(s.id(), encoded); err != nil {
		return fmt.Errorf("retain original publication evidence before replacement: %w", err)
	}
	proof.InputJournalSHA256 = storage.SHA256Hex(encoded)
	return next.ValidatePublication()
}

// checkPrivacyInputReplay preserves owed evidence if its backing objects vanished.
func (s *sessionScan) checkPrivacyInputReplay(p state.PendingPublication) error {
	if p.Commit == nil || p.Commit.Privacy == nil {
		return nil
	}
	if s.exactLocalPrivacySuccessor(p) {
		return nil
	}
	proof := p.Commit.Privacy
	if proof.InputJournalSHA256 != "" {
		j, raw, err := s.readPrivacyInputJournal()
		if err != nil {
			return err
		}
		if !privacyJournalMatches(j, raw, p) {
			return errors.New("privacy original evidence does not bind exact pending successor")
		}
		return s.verifyJournalReferences(j.Original)
	}
	input := proof.ReplayInput
	if input == nil {
		input = proof.PendingMutation
	}
	if input == nil || len(input.MetadataBytes) == 0 {
		return nil
	}
	// Original metadata is authenticated by ValidatePublication. Stage backing is
	// admissible only for its exact original source; otherwise verify every ref.
	original := state.PendingPublication{MetadataBytes: input.MetadataBytes, AdmissionStage: p.AdmissionStage}
	var m archive.Metadata
	if err := json.Unmarshal(input.MetadataBytes, &m); err != nil {
		return err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return err
	}

	if p.AdmissionStage != "" {
		manifest, _, err := s.local.ReadAdmissionStage(s.id(), p.AdmissionStage)
		if err == nil && state.CheckAdmissionStageOwnership(s.reg, manifest) == nil && manifest.SHA256 == m.SourceBundle.SHA256 && len(refs) == 1 {
			return nil
		}
		if err != nil {
			return err
		}
	}
	// Resolve the exact original selection, independent of current pending bytes.
	return s.verifyPrivacyOriginalRemote(original.MetadataBytes)
}

func (s *sessionScan) cleanupPrivacyInput(p state.PendingPublication) error {
	if p.Commit == nil || p.Commit.Privacy == nil || p.Commit.Privacy.InputJournalSHA256 == "" {
		return nil
	}
	prior := s.published.PublicationPredecessor()
	if prior.State != state.PredecessorPresent || storage.SHA256Hex(prior.Body) != p.Commit.MetadataSHA256 {
		return errors.New("original privacy evidence needs exact local selecting successor before cleanup")
	}
	if _, err := s.published.CommittedSources(); err != nil {
		return err
	}
	j, raw, err := s.readPrivacyInputJournal()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !privacyJournalMatches(j, raw, p) {
		return errors.New("privacy cleanup journal differs from exact local successor")
	}
	return s.local.RemovePublicationEvidence(s.id())
}

func privacyJournalMatches(j privacyInputJournal, raw []byte, p state.PendingPublication) bool {
	proof := p.Commit.Privacy
	exact := storage.SHA256Hex(raw) == proof.InputJournalSHA256 && j.ReplacementMetadataSHA256 == p.Commit.MetadataSHA256 && j.ReplacementSetSHA256 == p.Commit.SourceSetSHA256
	prepared := j.PreviousJournalSHA256 == proof.InputJournalSHA256 && j.PreviousReplacementMetadataSHA256 == p.Commit.MetadataSHA256 && j.PreviousReplacementSetSHA256 == p.Commit.SourceSetSHA256
	return exact || prepared
}

func (s *sessionScan) verifyPrivacyOriginalRemote(raw []byte) error {
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return err
	}
	sources := make([]storage.SourcePublication, len(refs))
	for i, ref := range refs {
		sources[i] = storage.SourcePublication{Key: ref.Key, SHA256: ref.SHA256, Size: ref.CompressedBytes}
	}
	return storage.VerifySourceSet(s.ctx, s.remote, sources, s.opts.Retry)
}

func (s *sessionScan) exactLocalPrivacySuccessor(p state.PendingPublication) bool {
	prior := s.published.PublicationPredecessor()
	if prior.State != state.PredecessorPresent || storage.SHA256Hex(prior.Body) != p.Commit.MetadataSHA256 {
		return false
	}
	refs, err := s.published.CommittedSources()
	if err != nil || len(refs) != len(p.Sources) {
		return false
	}
	for i, ref := range refs {
		if ref != p.Sources[i].Reference {
			return false
		}
	}
	return true
}

func (s *sessionScan) verifyJournalReferences(original state.PendingPublication) error {
	var refs []storage.SourcePublication
	for i, source := range original.Sources {
		payload := source.Bytes
		if i == 0 {
			payload = original.SourceBytes
		}
		if len(payload) > 0 {
			continue
		}
		refs = append(refs, storage.SourcePublication{Key: source.Reference.Key, SHA256: source.Reference.SHA256, Size: source.Reference.CompressedBytes})
	}
	if len(refs) == 0 {
		return nil
	}
	return storage.VerifySourceSet(s.ctx, s.remote, refs, s.opts.Retry)
}

func (s *sessionScan) privacyInputRetiredReferences(p state.PendingPublication) ([]archive.SourceReference, error) {
	if p.Commit == nil || p.Commit.Privacy == nil {
		return nil, nil
	}
	proof := p.Commit.Privacy
	if proof.InputJournalSHA256 != "" {
		j, raw, err := s.readPrivacyInputJournal()
		if errors.Is(err, os.ErrNotExist) && s.exactLocalPrivacySuccessor(p) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !privacyJournalMatches(j, raw, p) {
			return nil, errors.New("privacy retirement input differs")
		}
		refs := make([]archive.SourceReference, len(j.Original.Sources))
		for i, source := range j.Original.Sources {
			refs[i] = source.Reference
		}
		return refs, nil
	}
	input := proof.ReplayInput
	if input == nil {
		input = proof.PendingMutation
	}
	if input == nil || len(input.MetadataBytes) == 0 {
		return nil, nil
	}
	var m archive.Metadata
	if err := json.Unmarshal(input.MetadataBytes, &m); err != nil {
		return nil, err
	}
	return m.SourceReferences()
}

// requireNoOrphanPrivacyInput guards all absent-pending maintenance routes.
func (s *sessionScan) requireNoOrphanPrivacyInput() error {
	owed, err := s.published.Outstanding(s.reg, false)
	if err != nil {
		return err
	}
	if owed.Upload {
		return errors.New("retained original publication evidence requires successor journal recovery; native substitution is forbidden")
	}
	return nil
}

func (s *sessionScan) oldestPrivacyInput(p state.PendingPublication) (*state.PendingPublication, error) {
	e := p.Commit.Privacy
	if e.InputJournalSHA256 != "" {
		j, raw, err := s.readPrivacyInputJournal()
		if err != nil {
			return nil, err
		}
		if !privacyJournalMatches(j, raw, p) {
			return nil, errors.New("oldest privacy input journal differs")
		}
		if e.ReplayInput != nil && j.Original.Commit.MetadataSHA256 == e.ReplayInput.MetadataSHA256 && j.Original.Commit.SourceSetSHA256 == e.ReplayInput.SourceSetSHA256 {
			return &j.Original, nil
		}
	}
	input := e.ReplayInput
	if input == nil {
		return nil, errors.New("oldest privacy input authority is missing")
	}
	var m archive.Metadata
	if err := json.Unmarshal(input.MetadataBytes, &m); err != nil {
		return nil, err
	}
	refs, err := m.SourceReferences()
	if err != nil {
		return nil, err
	}
	overlay := pendingSourceStore{ObjectStore: s.remote, payloads: map[string][]byte{}}
	live, err := s.unreleasedPrivacyStage(p)
	if err != nil {
		return nil, err
	}
	if live {
		manifest, bundle, err := s.local.ReadAdmissionStage(s.id(), p.AdmissionStage)
		if err != nil {
			return nil, err
		}
		if err := state.CheckAdmissionStageOwnership(s.reg, manifest); err != nil {
			return nil, err
		}
		if manifest.SHA256 == m.SourceBundle.SHA256 {
			compressed, err := archive.BuildCompressedSource(bundle)
			if err != nil {
				return nil, err
			}
			overlay.payloads[m.SourceBundle.Key] = compressed.Bytes
		}
	}
	bundle, err := reader.LoadSource(s.ctx, overlay, m, reader.Limits{MaxCompressedBytes: 128 << 20, MaxUncompressedBytes: 128 << 20})
	if err != nil {
		return nil, err
	}
	skill := ""
	if input.Privacy != nil && len(input.Privacy.Sources) > 0 {
		skill = input.Privacy.Sources[0].NewPolicy.Skill
	}
	root := &state.PendingPublication{SkillEvidence: skill, Bundle: bundle, MetadataBytes: input.MetadataBytes, MetadataKey: p.MetadataKey, SourceKey: refs[0].Key, SourceSHA256: refs[0].SHA256, SourceSize: refs[0].CompressedBytes, MetadataOnly: true, AdmissionStage: p.AdmissionStage,
		Commit: &state.PublicationCommit{Version: 1, MetadataSHA256: input.MetadataSHA256, SourceSetSHA256: input.SourceSetSHA256, PolicyContext: input.PolicyContext, Purpose: input.Purpose, Predecessor: input.Predecessor, PredecessorSHA256: input.PredecessorSHA256, Continuity: input.Continuity, Privacy: input.Privacy, DestinationID: p.Commit.DestinationID, AdmissionContext: p.Commit.AdmissionContext}}
	for _, ref := range refs {
		root.Sources = append(root.Sources, state.PublicationSource{Reference: ref})
	}

	if err := root.ValidatePublication(); err != nil {
		return nil, err
	}
	return root, nil
}

// unreleasedPrivacyStage distinguishes immutable live backing from completed
// cleanup. Completed stages must resolve originals through verified remote refs.
func (s *sessionScan) unreleasedPrivacyStage(p state.PendingPublication) (bool, error) {
	if p.AdmissionStage == "" {
		return false, nil
	}
	if p.AdmissionStage != s.reg.AdmissionStage {
		return false, state.ErrAdmissionStageRecovery
	}
	released, err := s.local.AdmissionStageReleased(s.reg)
	return !released, err
}

// oldestRetainedLoader maps only the exact immediate role/revision/age to its
// authenticated oldest source. Decoded originals are not retained by the loader.
func (s *sessionScan) oldestRetainedLoader(immediate, root state.PendingPublication) (retainedSourceLoader, error) {
	var before, oldest archive.Metadata
	if err := json.Unmarshal(immediate.MetadataBytes, &before); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(root.MetadataBytes, &oldest); err != nil {
		return nil, err
	}
	selected, err := state.RevisionSelections(before)
	if err != nil {
		return nil, err
	}
	originals, err := state.RevisionSelections(oldest)
	if err != nil || len(originals) != len(selected) {
		return nil, errors.New("oldest privacy source selection differs")
	}
	mapping := make(map[archive.RevisionReference]archive.RevisionReference, len(selected))
	for i, ref := range selected {
		if ref.RevisionID != originals[i].RevisionID || !ref.CapturedAt.Equal(originals[i].CapturedAt) {
			return nil, errors.New("oldest privacy revision role or age differs")
		}
		mapping[ref] = originals[i]
	}
	load, err := s.pendingRetainedLoader(root)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, ref archive.RevisionReference) (archive.SourceBundle, error) {
		old, ok := mapping[ref]
		if !ok {
			return archive.SourceBundle{}, errors.New("immediate privacy reference is not authenticated")
		}
		return load(ctx, old)
	}, nil
}
