package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

const publicationEvidenceLimit int64 = 512 << 20

// PublicationOriginalRole retains exact original bytes, never selecting authority.
type PublicationOriginalRole struct {
	Version             int    `json:"version"`
	Kind                string `json:"kind"`
	FrozenContextSHA256 string `json:"frozen_context_sha256"`
	MetadataSHA256      string `json:"metadata_sha256"`
	InputMapSHA256      string `json:"input_map_sha256"`
	ProtectionMapSHA256 string `json:"protection_map_sha256"`
	Raw                 []byte `json:"raw"`
	SHA256              string `json:"sha256"`
	Size                int64  `json:"size"`
}

// EvidenceTarget binds immutable preparation or exact ready selection.
type EvidenceTarget struct {
	Phase             string `json:"phase"`
	PreparationSHA256 string `json:"preparation_sha256,omitempty"`
	MetadataSHA256    string `json:"metadata_sha256,omitempty"`
	SourceSetSHA256   string `json:"source_set_sha256,omitempty"`
	LegacyFileSHA256  string `json:"legacy_file_sha256,omitempty"`
}

// EvidenceReplacement retains only the current and immediate crash predecessor.
type EvidenceReplacement struct {
	Version                int            `json:"version"`
	Target                 EvidenceTarget `json:"target"`
	PreviousTarget         EvidenceTarget `json:"previous_target"`
	PreviousEvidenceSHA256 string         `json:"previous_evidence_sha256,omitempty"`
}

// PublicationOriginalEvidence is one nonselecting original-evidence journal.
type PublicationOriginalEvidence struct {
	Version          int                      `json:"version"`
	Kind             string                   `json:"kind"`
	SessionID        string                   `json:"session_id"`
	OwnerSHA256      string                   `json:"owner_sha256"`
	DestinationID    string                   `json:"destination_id"`
	AdmissionContext string                   `json:"admission_context"`
	MigrationOrigin  *PublicationOriginalRole `json:"migration_origin,omitempty"`
	PrivacyOrigin    *PublicationOriginalRole `json:"privacy_origin,omitempty"`
	Link             EvidenceReplacement      `json:"link"`
	SHA256           string                   `json:"sha256"`
}

func originalEvidenceSHA(e PublicationOriginalEvidence) string {
	type roleBinding struct {
		Version                                                                                int
		Kind, FrozenContextSHA256, MetadataSHA256, InputMapSHA256, ProtectionMapSHA256, SHA256 string
		Size                                                                                   int64
	}
	binding := func(r *PublicationOriginalRole) *roleBinding {
		if r == nil {
			return nil
		}
		return &roleBinding{r.Version, r.Kind, r.FrozenContextSHA256, r.MetadataSHA256, r.InputMapSHA256, r.ProtectionMapSHA256, r.SHA256, r.Size}
	}
	raw, _ := json.Marshal(struct {
		Version                                                       int
		Kind, SessionID, OwnerSHA256, DestinationID, AdmissionContext string
		MigrationOrigin, PrivacyOrigin                                *roleBinding
		Link                                                          EvidenceReplacement
	}{e.Version, e.Kind, e.SessionID, e.OwnerSHA256, e.DestinationID, e.AdmissionContext, binding(e.MigrationOrigin), binding(e.PrivacyOrigin), e.Link})
	return publicationSHA256(append([]byte("publication-original-evidence/v2\x00"), raw...))
}

func evidenceTarget(p PendingPublication, raw []byte) EvidenceTarget {
	if p.JournalVersion == 0 {
		phase := "ready"
		if p.History != nil && p.History.Preparing {
			phase = "preparing"
		}
		return EvidenceTarget{Phase: phase, LegacyFileSHA256: publicationSHA256(raw)}
	}
	target := EvidenceTarget{Phase: p.Phase, PreparationSHA256: p.Preparation.SHA256}
	if p.Phase == "ready" && p.Commit != nil {
		target.MetadataSHA256 = p.Commit.MetadataSHA256
		target.SourceSetSHA256 = p.Commit.SourceSetSHA256
	}
	return target
}

func validEvidenceTarget(t EvidenceTarget) bool {
	if t.Phase != "preparing" && t.Phase != "ready" {
		return false
	}
	if t.LegacyFileSHA256 != "" {
		return validPublicationDigest(t.LegacyFileSHA256) && t.PreparationSHA256 == "" && t.MetadataSHA256 == "" && t.SourceSetSHA256 == ""
	}
	if !validPublicationDigest(t.PreparationSHA256) {
		return false
	}
	if t.Phase == "preparing" {
		return t.MetadataSHA256 == "" && t.SourceSetSHA256 == ""
	}
	return validPublicationDigest(t.MetadataSHA256) && validPublicationDigest(t.SourceSetSHA256)
}

func originalRole(p PendingPublication, raw []byte, destination, admission string) (*PublicationOriginalRole, error) {
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		return nil, err
	}
	sources, err := selectedPublicationSources(p, destination, admission)
	if err != nil {
		return nil, err
	}
	inputRaw, _ := json.Marshal(struct {
		Inputs                        []HistoryInput
		SkillEvidence                 string
		FilterVersion, AdapterVersion string
	}{func() []HistoryInput {
		if p.History == nil {
			return nil
		}
		return p.History.Inputs
	}(), p.SkillEvidence, p.Bundle.Capture.FilterVersion, p.Bundle.Capture.AdapterVersion})
	kind := "sealed-pending-v1"
	if p.History != nil && p.History.Preparing {
		kind = "history-preparing-v1"
	}
	return &PublicationOriginalRole{Version: 1, Kind: kind, FrozenContextSHA256: publicationOwner(metadata, destination, admission), MetadataSHA256: publicationSHA256(p.MetadataBytes), InputMapSHA256: publicationSHA256(append([]byte("original-input-map/v1\x00"), inputRaw...)), ProtectionMapSHA256: payloadSetSHA(sources), Raw: raw, SHA256: publicationSHA256(raw), Size: int64(len(raw))}, nil
}

func (s *Store) validateOriginalEvidence(e PublicationOriginalEvidence, id string) error {
	total := int64(0)
	for _, role := range []*PublicationOriginalRole{e.MigrationOrigin, e.PrivacyOrigin} {
		if role != nil {
			if role.Size < 0 || role.Size > publicationEvidenceLimit-total {
				return ErrDurableStorageRecovery
			}
			total += role.Size
		}
	}
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(total) {
			return errStateBudget
		}
		defer s.resourceBudget.Release(total)
	}
	return e.validate(id)
}
func (e PublicationOriginalEvidence) validate(id string) error {
	if e.Version != 2 || e.Kind != "publication-original-evidence" || e.SessionID != id || e.Link.Version != 1 || !validEvidenceTarget(e.Link.Target) || !validEvidenceTarget(e.Link.PreviousTarget) || e.Link.PreviousEvidenceSHA256 != "" && !validPublicationDigest(e.Link.PreviousEvidenceSHA256) || e.SHA256 != originalEvidenceSHA(e) || e.MigrationOrigin == nil && e.PrivacyOrigin == nil {
		return ErrDurableStorageRecovery
	}
	total := int64(0)
	for i, role := range []*PublicationOriginalRole{e.MigrationOrigin, e.PrivacyOrigin} {
		if role == nil {
			continue
		}
		if role.Version != 1 || (role.Kind != "sealed-pending-v1" && role.Kind != "history-preparing-v1") {
			return ErrDurableStorageRecovery
		}
		if role.Size != int64(len(role.Raw)) || role.Size <= 0 || role.SHA256 != publicationSHA256(role.Raw) || role.Size > publicationEvidenceLimit-total {
			return ErrDurableStorageRecovery
		}
		total += role.Size
		var pending PendingPublication
		if err := json.Unmarshal(role.Raw, &pending); err != nil {
			return err
		}
		if pending.Bundle.ArchiveSessionID != id {
			return ErrDurableStorageRecovery
		}
		rebuilt, err := originalRole(pending, role.Raw, e.DestinationID, e.AdmissionContext)
		if err != nil {
			return err
		}
		if rebuilt.Kind != role.Kind || rebuilt.FrozenContextSHA256 != role.FrozenContextSHA256 || rebuilt.MetadataSHA256 != role.MetadataSHA256 || rebuilt.InputMapSHA256 != role.InputMapSHA256 || rebuilt.ProtectionMapSHA256 != role.ProtectionMapSHA256 || rebuilt.FrozenContextSHA256 != e.OwnerSHA256 {
			return ErrDurableStorageRecovery
		}
		if i == 0 && pending.JournalVersion != 0 {
			return ErrDurableStorageRecovery
		}
		if i == 1 && (pending.Commit == nil || pending.ValidatePublication() != nil) {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func (s *Store) rootedPublicationBytes(g config.DurableStorageGuard, path string, limit int64) ([]byte, func(), error) {
	home, err := g.RootedHome(s.home)
	if err != nil {
		return nil, nil, err
	}
	before, err := home.Root.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	n := before.Size()
	if !before.Mode().IsRegular() || n < 0 || n > limit {
		return nil, nil, ErrDurableStorageRecovery
	}
	release := func() {}
	if s.resourceBudget != nil {
		if !s.resourceBudget.Reserve(2 * n) {
			return nil, nil, errStateBudget
		}
		release = func() { s.resourceBudget.Release(2 * n) }
	}
	fail := func(e error) ([]byte, func(), error) { release(); return nil, nil, e }
	f, err := home.Root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(before, opened) {
		return fail(errors.Join(ErrDurableStorageRecovery, err))
	}
	raw, err := io.ReadAll(io.LimitReader(f, n+1))
	if err != nil || int64(len(raw)) != n {
		return fail(errors.Join(ErrDurableStorageRecovery, err))
	}
	after, err := home.Root.Lstat(path)
	if err != nil || !sameDurableStamp(before, after) {
		return fail(errors.Join(ErrDurableStorageRecovery, err))
	}
	if err = g.CheckHome(s.home); err != nil {
		return fail(err)
	}
	if err = s.durableContext().Err(); err != nil {
		return fail(err)
	}
	return raw, release, nil
}

func evidencePath(id string) string {
	return filepath.Join("publication-evidence", id, "original.json")
}

// savePublicationPendingGuard preserves a supported legacy journal before its
// first protocol2 replacement, and validates every later crash-link advancement.
func (s *Store) savePublicationPendingGuard(g config.DurableStorageGuard, id string, p PendingPublication) error {
	path := filepath.Join("pending", id+".json")
	old, release, err := s.rootedPublicationBytes(g, path, publicationEvidenceLimit)
	if errors.Is(err, os.ErrNotExist) {
		return s.savePendingGuard(s.durableContext(), g, id, p)
	}
	if err != nil {
		return err
	}
	defer release()
	var previous PendingPublication
	if err = json.Unmarshal(old, &previous); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	target := evidenceTarget(p, nil)
	raw, closeEvidence, readErr := s.rootedPublicationBytes(g, evidencePath(id), publicationEvidenceLimit)
	var evidence PublicationOriginalEvidence
	if readErr == nil {
		defer closeEvidence()
		if err = closedPublicationDecode(raw, &evidence); err != nil {
			return err
		}
		if err = s.validateOriginalEvidence(evidence, id); err != nil {
			return err
		}
		if evidence.DestinationID != p.Preparation.DestinationID || evidence.AdmissionContext != p.Preparation.AdmissionContext {
			return ErrDurableStorageRecovery
		}
		installed := evidenceTarget(previous, old)
		if installed != evidence.Link.Target {
			if installed != evidence.Link.PreviousTarget || target != evidence.Link.Target {
				return ErrDurableStorageRecovery
			}
			return s.savePendingGuard(s.durableContext(), g, id, p)
		}
		if target == installed {
			return s.savePendingGuard(s.durableContext(), g, id, p)
		}
		evidence.Link = EvidenceReplacement{Version: 1, Target: target, PreviousTarget: installed, PreviousEvidenceSHA256: evidence.SHA256}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	} else if previous.JournalVersion == 0 {
		role, e := originalRole(previous, old, p.Preparation.DestinationID, p.Preparation.AdmissionContext)
		if e != nil {
			return e
		}
		evidence = PublicationOriginalEvidence{Version: 2, Kind: "publication-original-evidence", SessionID: id, OwnerSHA256: role.FrozenContextSHA256, DestinationID: p.Preparation.DestinationID, AdmissionContext: p.Preparation.AdmissionContext, MigrationOrigin: role, Link: EvidenceReplacement{Version: 1, Target: target, PreviousTarget: evidenceTarget(previous, old)}}
	} else {
		return s.savePendingGuard(s.durableContext(), g, id, p)
	}
	evidence.SHA256 = originalEvidenceSHA(evidence)
	if err = s.validateOriginalEvidence(evidence, id); err != nil {
		return err
	}
	if err = s.writeDurableGuard(s.durableContext(), g, evidencePath(id), evidence); err != nil {
		return err
	}
	return s.savePendingGuard(s.durableContext(), g, id, p)
}

// LoadPublicationPending is the publication owner's closed migration replay.
// Generic native/deletion eligibility retains its opaque-evidence refusal.
func (s *Store) LoadPublicationPending(id string) (pending PendingPublication, found bool, err error) {
	pending, found, err = s.LoadPending(id)
	if err == nil {
		return pending, found, nil
	}
	owed, probe := s.hasPublicationEvidence(id)
	if probe != nil || !owed {
		return pending, found, errors.Join(err, probe)
	}
	cfg, present, e := config.Load(s.home)
	if e != nil || !present || !cfg.PublicationCompositionProtection {
		return PendingPublication{}, true, errors.Join(ErrDurableStorageRecovery, e)
	}
	if !safeFileComponent(id) {
		return PendingPublication{}, true, ErrDurableStorageRecovery
	}
	err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
		home, e := g.RootedHome(s.home)
		if e != nil {
			return e
		}
		if e = s.validatePublicationEvidenceDirectory(home.Root, id); e != nil {
			return e
		}
		raw, release, e := s.rootedPublicationBytes(g, evidencePath(id), publicationEvidenceLimit)
		if e != nil {
			return e
		}
		defer release()
		var evidence PublicationOriginalEvidence
		if e = closedPublicationDecode(raw, &evidence); e != nil {
			return e
		}
		if e = s.validateOriginalEvidence(evidence, id); e != nil {
			return e
		}
		body, closeBody, e := s.rootedPublicationBytes(g, filepath.Join("pending", id+".json"), publicationEvidenceLimit)
		if e != nil {
			return e
		}
		defer closeBody()

		if e = json.Unmarshal(body, &pending); e != nil {
			return e
		}
		if pending.Bundle.ArchiveSessionID != id {
			return ErrDurableStorageRecovery
		}
		target := evidenceTarget(pending, body)
		if target != evidence.Link.Target && target != evidence.Link.PreviousTarget {
			return ErrDurableStorageRecovery
		}
		if pending.JournalVersion == 2 && (pending.Preparation.DestinationID != evidence.DestinationID || pending.Preparation.AdmissionContext != evidence.AdmissionContext || pending.Preparation.OwnerSHA256 != evidence.OwnerSHA256) {
			return ErrDurableStorageRecovery
		}
		if e = s.validateReadablePending(pending); e != nil {
			return e
		}
		if e = pending.ValidateHistoryBudgeted(id, s.resourceBudget); e != nil {
			return e
		}
		if s.resourceBudget != nil {
			n := int64(len(body))
			if !s.resourceBudget.Reserve(n) {
				return errStateBudget
			}
			*s.resourceReleases = append(*s.resourceReleases, func() { s.resourceBudget.Release(n) })
		}
		return nil
	})
	return pending, true, errors.Join(err)
}

// SettlePublicationMigration removes only the migration role after a full local
// selecting commit. Privacy-original discharge remains its lifecycle owner's work.
func (s *Store) SettlePublicationMigration(id string, p PendingPublication) error {
	owed, err := s.hasPublicationEvidence(id)
	if err != nil || !owed {
		return err
	}
	return config.WithPublicationComposition(s.home, func(composition config.PublicationCompositionGuard) error {
		g, err := composition.Storage(s.home)
		if err != nil {
			return err
		}
		raw, release, err := s.rootedPublicationBytes(g, evidencePath(id), publicationEvidenceLimit)
		if err != nil {
			return err
		}
		defer release()
		var evidence PublicationOriginalEvidence
		if err = closedPublicationDecode(raw, &evidence); err != nil {
			return err
		}
		if err = s.validateOriginalEvidence(evidence, id); err != nil {
			return err
		}
		publishedRaw, closePublished, err := s.rootedPublicationBytes(g, filepath.Join("published", id+".json"), publicationEvidenceLimit)
		if err != nil {
			return err
		}
		defer closePublished()
		var published publishedState
		if err = json.Unmarshal(publishedRaw, &published); err != nil {
			return err
		}
		if published.Commit == nil || p.Commit == nil || published.Commit.MetadataSHA256 != p.Commit.MetadataSHA256 || published.Commit.SourceSetSHA256 != p.Commit.SourceSetSHA256 || published.Commit.PayloadSetSHA256 != p.Commit.PayloadSetSHA256 || evidence.DestinationID != p.Commit.DestinationID || evidence.AdmissionContext != p.Commit.AdmissionContext {
			return ErrDurableStorageRecovery
		}
		if p.JournalVersion != 2 || p.Phase != "ready" || p.ValidatePublication() != nil || evidence.Link.Target != evidenceTarget(p, nil) {
			return ErrDurableStorageRecovery
		}
		evidence.MigrationOrigin = nil
		if evidence.PrivacyOrigin != nil {
			evidence.SHA256 = originalEvidenceSHA(evidence)
			return s.writeDurableGuard(s.durableContext(), g, evidencePath(id), evidence)
		}
		home, err := g.RootedHome(s.home)
		if err != nil {
			return err
		}
		dir, err := privateDirectory(home.Root, filepath.Dir(evidencePath(id)), false)
		if err != nil {
			return err
		}
		defer dir.Close()
		file, err := dir.Open(".")
		if err != nil {
			return err
		}
		entries, e := file.ReadDir(2)
		_ = file.Close()
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		if len(entries) != 1 || entries[0].Name() != "original.json" {
			return ErrDurableStorageRecovery
		}
		if err = g.CheckHome(s.home); err != nil {
			return err
		}
		if err = dir.Remove("original.json"); err != nil {
			return err
		}
		syncDir, err := dir.Open(".")
		if err != nil {
			return err
		}
		defer syncDir.Close()
		return syncDir.Sync()
	})
}

// Evidence is exactly one original control file. Anonymous siblings cannot be
// interpreted or ignored by an owning replay or a native eligibility guard.
func (s *Store) validatePublicationEvidenceDirectory(root *os.Root, id string) error {
	const charge = 64 << 10
	if s.resourceBudget != nil && !s.resourceBudget.Reserve(charge) {
		return errStateBudget
	}
	if s.resourceBudget != nil {
		defer s.resourceBudget.Release(charge)
	}
	dir, err := privateDirectory(root, filepath.Join("publication-evidence", id), false)
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := file.ReadDir(2)
	err = errors.Join(readErr, file.Close())
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != "original.json" {
		return ErrDurableStorageRecovery
	}
	return nil
}
