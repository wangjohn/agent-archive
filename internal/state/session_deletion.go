package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// ErrRemovalPending identifies an authorized removal awaiting completion.
var ErrRemovalPending = errors.New("authorized session removal is unfinished")

// ErrDeletionWorkChanged requires a fresh decision after newer queued work.
var ErrDeletionWorkChanged = errors.New("session work changed before removal; retry after processing the newer request")

// SessionDeletionPhase identifies a durable removal transition.
type SessionDeletionPhase string

const (
	// DeletionPrepared identifies the prepared removal phase.
	DeletionPrepared SessionDeletionPhase = "prepared"
	// DeletionDeleting identifies the deleting removal phase.
	DeletionDeleting SessionDeletionPhase = "deleting"
	// DeletionAbsent identifies the absent removal phase.
	DeletionAbsent SessionDeletionPhase = "absent"
	// DeletionCleaned identifies the cleaned removal phase.
	DeletionCleaned SessionDeletionPhase = "cleaned"
	// DeletionRestoring identifies the restoring removal phase.
	DeletionRestoring SessionDeletionPhase = "restoring"
	// DeletionRestored identifies the restored removal phase.
	DeletionRestored SessionDeletionPhase = "restored"
)

// SessionDeletion retains exact whole-session deletion authority across crashes.
// It is not permission to publish; only retention may authorize a newer hook.
type SessionDeletion struct {
	CoveredRequest    bool                      `json:"covered_request,omitempty"`
	LocalRemoved      bool                      `json:"local_removed,omitempty"`
	Version           int                       `json:"version"`
	Owner             string                    `json:"owner"`
	Reason            RemovalReason             `json:"reason"`
	Phase             SessionDeletionPhase      `json:"phase"`
	MetadataSHA256    string                    `json:"metadata_sha256,omitempty"`
	References        []archive.SourceReference `json:"references,omitempty"`
	RequestToken      string                    `json:"request_token,omitempty"`
	At                time.Time                 `json:"at"`
	RestorationSHA256 string                    `json:"restoration_sha256,omitempty"`
	RestorationToken  string                    `json:"restoration_token,omitempty"`
	Checksum          string                    `json:"checksum"`
}

func deletionOwner(reg archive.SessionRegistration) string {
	raw, _ := json.Marshal(struct {
		ID          string    `json:"ID"`
		Native      string    `json:"Native"`
		Project     string    `json:"Project"`
		Harness     string    `json:"Harness"`
		Destination string    `json:"Destination"`
		Stage       string    `json:"Stage"`
		Admission   time.Time `json:"Admission"`
	}{reg.ArchiveSessionID, reg.NativeSessionID, reg.ProjectID, reg.Harness.Name, reg.DestinationID, reg.AdmissionStage, reg.Admitted()})
	return publicationSHA256(raw)
}

func deletionChecksum(j SessionDeletion) string {
	j.Checksum = ""
	b, _ := json.Marshal(j)
	return publicationSHA256(b)
}

func validateDeletionID(id string) error {
	if !safeFileComponent(id) {
		return ErrAdmissionStageRecovery
	}
	return nil
}

// LoadSessionDeletion refuses malformed authority without quarantining evidence.
func (s *Store) LoadSessionDeletion(reg archive.SessionRegistration) (SessionDeletion, bool, error) {
	err := validateDeletionID(reg.ArchiveSessionID)
	if err != nil {
		return SessionDeletion{}, false, err
	}
	b, err := s.readDeletionFile(reg.ArchiveSessionID)
	if errors.Is(err, os.ErrNotExist) {
		if temporary, e := s.hasDeletionTemporary(reg.ArchiveSessionID); e != nil || temporary {
			return SessionDeletion{}, true, errors.Join(ErrAdmissionStageRecovery, e)
		}
		return SessionDeletion{}, false, nil
	}
	if err != nil {
		return SessionDeletion{}, true, err
	}
	var j SessionDeletion
	if json.Unmarshal(b, &j) != nil || j.Version != 1 || j.Owner != deletionOwner(reg) || j.At.IsZero() || j.Checksum != deletionChecksum(j) || !canonicalDeletion(b, j) || (j.Reason != RemovalReasonRetention && j.Reason != RemovalReasonUndo) {
		return j, true, ErrAdmissionStageRecovery
	}
	if j.CoveredRequest && (j.Reason != RemovalReasonRetention || j.RequestToken == "") {
		return j, true, ErrAdmissionStageRecovery
	}
	if j.LocalRemoved && j.Phase != DeletionCleaned {
		return j, true, ErrAdmissionStageRecovery
	}
	switch j.Phase {
	case DeletionPrepared, DeletionDeleting, DeletionAbsent, DeletionCleaned, DeletionRestoring, DeletionRestored:
	default:
		return j, true, ErrAdmissionStageRecovery
	}
	if j.MetadataSHA256 != "" && !validPublicationDigest(j.MetadataSHA256) {
		return j, true, ErrAdmissionStageRecovery
	}
	if len(j.References) > archive.MaxPreservedRevisions+1 {
		return j, true, ErrAdmissionStageRecovery
	}
	for _, ref := range j.References {
		prefix := "sessions/" + reg.Harness.Name + "/" + reg.ArchiveSessionID + "/source."
		if !strings.HasPrefix(ref.Key, prefix) || !validPublicationDigest(ref.SHA256) || ref.Key != prefix+ref.SHA256+".jsonl.gz" || ref.CompressedBytes <= 0 || ref.CompressedBytes > 128<<20 {
			return j, true, ErrAdmissionStageRecovery
		}
	}
	if (j.MetadataSHA256 == "") != (len(j.References) == 0) {
		return j, true, ErrAdmissionStageRecovery
	}
	return j, true, nil
}

// PrepareSessionDeletion journals reviewed exact selecting metadata before removal.
func (s *Store) PrepareSessionDeletion(reg archive.SessionRegistration, reason RemovalReason, raw []byte, at time.Time) (SessionDeletion, error) {
	var expected *string
	if reason == RemovalReasonRetention {
		empty := ""
		expected = &empty
	}
	return s.prepareDeletionAtRequest(reg, reason, raw, at, expected)
}

// PrepareRetentionDeletion binds an already reviewed expiry decision to its
// exact queued token. The caller holds collector.lock; changed work defers.
func (s *Store) PrepareRetentionDeletion(reg archive.SessionRegistration, raw []byte, at time.Time, decisionToken string) (SessionDeletion, error) {
	return s.prepareDeletionAtRequest(reg, RemovalReasonRetention, raw, at, &decisionToken)
}

func (s *Store) prepareDeletionAtRequest(reg archive.SessionRegistration, reason RemovalReason, raw []byte, at time.Time, expected *string) (SessionDeletion, error) {
	if j, found, err := s.LoadSessionDeletion(reg); err != nil {
		return j, err
	} else if found {
		if reason == RemovalReasonUndo && j.Reason == RemovalReasonRetention {
			// Explicit removal revokes an unfinished retention restoration.
		} else if j.Phase == DeletionRestored && len(raw) > 0 {
			published, e := s.LoadPublishedState(reg.ArchiveSessionID)
			if e != nil {
				return j, e
			}
			if publicationSHA256(published.Metadata()) != publicationSHA256(raw) {
				return j, ErrAdmissionStageRecovery
			}
			if _, e = published.CommittedSources(); e != nil {
				return j, e
			}
		} else {
			return j, nil
		}
	}
	if reason != RemovalReasonRetention && reason != RemovalReasonUndo {
		return SessionDeletion{}, ErrAdmissionStageRecovery
	}
	j := SessionDeletion{Version: 1, Owner: deletionOwner(reg), Reason: reason, Phase: DeletionPrepared, At: at.UTC()}
	if len(raw) > 0 {
		published, e := s.LoadPublishedState(reg.ArchiveSessionID)
		if e != nil {
			return j, e
		}
		// An intact registered owner may remove a freshly verified remote selector
		// after losing its local publication cache. A known different selector
		// still refuses; this grants no restoration/publication authority.
		prior := published.PublicationPredecessor()
		if prior.State == PredecessorPresent && publicationSHA256(prior.Body) != publicationSHA256(raw) {
			return j, ErrAdmissionStageRecovery
		}
		if _, e = published.CommittedSources(); e != nil {
			return j, e
		}
		var m archive.Metadata
		if json.Unmarshal(raw, &m) != nil || m.SessionID != reg.ArchiveSessionID || m.NativeSessionID != reg.NativeSessionID || m.ProjectID != reg.ProjectID || m.Harness.Name != reg.Harness.Name {
			return j, ErrAdmissionStageRecovery
		}
		refs, err := m.SourceReferences()
		if err != nil {
			return j, err
		}
		j.References = refs
		j.MetadataSHA256 = publicationSHA256(raw)
	}
	req, found, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		return j, err
	}
	if expected != nil {
		j.RequestToken = *expected
		j.CoveredRequest = *expected != ""
	} else if found {
		j.RequestToken = req.Token
	}
	if expected != nil {
		return j, s.saveDeletionAtRequest(reg, j, *expected)
	}
	return j, s.saveSessionDeletion(reg, j)
}

func (s *Store) saveSessionDeletion(reg archive.SessionRegistration, j SessionDeletion) error {
	if err := validateDeletionID(reg.ArchiveSessionID); err != nil {
		return err
	}
	j.Checksum = deletionChecksum(j)
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return s.writeDeletionFile(reg.ArchiveSessionID, raw)
}

// AdvanceSessionDeletion durably advances only the existing exact deletion intent.
func (s *Store) AdvanceSessionDeletion(reg archive.SessionRegistration, phase SessionDeletionPhase) error {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found {
		return ErrAdmissionStageRecovery
	}
	allowed := j.Phase == phase || j.Phase == DeletionPrepared && phase == DeletionDeleting || j.Phase == DeletionDeleting && phase == DeletionAbsent || j.Phase == DeletionAbsent && (phase == DeletionCleaned || phase == DeletionRestored) || j.Phase == DeletionCleaned && phase == DeletionRestored
	if !allowed {
		return ErrAdmissionStageRecovery
	}
	j.Phase = phase
	return s.saveSessionDeletion(reg, j)
}

// RestoreAfterRetention reseals complete retained evidence only against an exact
// proven retention transition and a newer hook. Remote absence must be checked
// by the caller under the collector lock; undo never grants this authority.
func (s *Store) RestoreAfterRetention(reg archive.SessionRegistration, p PendingPublication) (PendingPublication, bool, error) {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found {
		return p, false, err
	}
	if j.Phase == DeletionRestored {
		return p, false, nil
	}
	if j.LocalRemoved || j.Reason != RemovalReasonRetention || (j.Phase != DeletionAbsent && j.Phase != DeletionCleaned && j.Phase != DeletionRestoring) || p.Commit == nil || p.Commit.Predecessor != PredecessorPresent || p.Commit.PredecessorSHA256 != j.MetadataSHA256 || p.Commit.DestinationID != reg.DestinationID || p.Commit.Purpose != PublicationCapture || p.RequestToken == "" || p.RequestToken == j.RequestToken {
		return p, false, ErrAdmissionStageRecovery
	}
	if err = p.ValidatePublication(); err != nil {
		return p, false, err
	}
	req, have, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		return p, false, err
	}
	if !have || req.Token != p.RequestToken || req.RequestedAt.Before(j.At) {
		return p, false, ErrAdmissionStageRecovery
	}
	hook := false
	for _, reason := range req.Reasons {
		if reason != "backfill" {
			hook = true
		}
	}
	if !hook {
		return p, false, ErrAdmissionStageRecovery
	}
	c := *p.Commit
	p.Commit = nil
	next, err := PreparePublication(p, PublicationPredecessor{State: PredecessorAbsent}, c.DestinationID, c.AdmissionContext, c.PolicyContext, c.Purpose)
	if err != nil {
		return p, false, err
	}
	if j.Phase == DeletionRestoring && (j.RestorationSHA256 != next.Commit.MetadataSHA256 || j.RestorationToken != p.RequestToken) {
		return p, false, ErrAdmissionStageRecovery
	}
	next.Commit.Retention = &RetentionRestoration{DeletionSHA256: deletionIntentSHA(j), OwnerSHA256: j.Owner, ReplacementSHA256: next.Commit.MetadataSHA256, PredecessorSHA256: j.MetadataSHA256, CoveredToken: p.RequestToken}
	j.Phase = DeletionRestoring
	j.RestorationSHA256 = next.Commit.MetadataSHA256
	j.RestorationToken = p.RequestToken
	if err = s.saveSessionDeletion(reg, j); err != nil {
		return p, false, err
	}
	return next, true, nil
}

// CompleteRetentionRestoration closes only the exact locally committed replacement.
func (s *Store) CompleteRetentionRestoration(reg archive.SessionRegistration, p PendingPublication, published *Published) error {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found {
		return err
	}
	if j.Phase != DeletionRestoring {
		return nil
	}
	if p.Commit == nil || p.Commit.MetadataSHA256 != j.RestorationSHA256 || p.RequestToken != j.RestorationToken || publicationSHA256(published.Metadata()) != j.RestorationSHA256 {
		return ErrAdmissionStageRecovery
	}
	if _, err = published.CommittedSources(); err != nil {
		return err
	}
	j.Phase = DeletionRestored
	return s.saveSessionDeletion(reg, j)
}

// DeletionCaptureAllowed prevents interrupted removal from resurrecting an
// explicitly removed session or substituting native bytes for missing evidence.
func (s *Store) DeletionCaptureAllowed(reg archive.SessionRegistration, req Request) error {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found {
		return err
	}
	if j.LocalRemoved {
		return ErrAdmissionStageRecovery
	}
	if j.Phase == DeletionRestored {
		return nil
	}
	if j.Reason != RemovalReasonRetention || j.Phase == DeletionPrepared || j.Phase == DeletionDeleting {
		return ErrRemovalPending
	}
	if j.Phase == DeletionRestoring {
		pending, found, err := s.LoadPending(reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		if !found || pending.Commit == nil || pending.Commit.MetadataSHA256 != j.RestorationSHA256 || pending.RequestToken != j.RestorationToken {
			return ErrAdmissionStageRecovery
		}
		return nil
	}
	if req.Token == "" || req.Token == j.RequestToken || req.RequestedAt.Before(j.At) {
		return ErrAdmissionStageRecovery
	}
	for _, reason := range req.Reasons {
		if reason != "backfill" {
			return nil
		}
	}
	return ErrAdmissionStageRecovery
}

// HasDurableSessionEvidence cheaply recognizes admitted or prepared stage
// obligations even after ownership was lost. No orphan deletion is authorized.
func (s *Store) HasDurableSessionEvidence(id string) (bool, error) {
	if owed, err := s.HasPending(id); err != nil || owed {
		return owed, err
	}
	if !safeFileComponent(id) {
		return true, ErrAdmissionStageRecovery
	}
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return true, err
	}
	defer func() { _ = root.Close() }()
	for _, directory := range []string{admissionStageDir, "session-deletions"} {
		info, err := root.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return true, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return true, ErrAdmissionStageRecovery
		}
		rooted := *s
		rooted.quotaRoot = root
		entries, err := rooted.quotaReadDir(filepath.Join(s.home, directory))
		if err != nil {
			return true, err
		}
		for _, entry := range entries {
			if directory == admissionStageDir && ownedStageFilename(id, entry.Name()) || directory == "session-deletions" && (entry.Name() == id+".json" || strings.HasPrefix(entry.Name(), id+"-")) {
				return true, nil
			}
		}
	}
	return false, nil
}

// RetentionRestoration binds an absent replay to the exact original deletion.
type RetentionRestoration struct {
	DeletionSHA256    string `json:"deletion_sha256"`
	OwnerSHA256       string `json:"owner_sha256"`
	ReplacementSHA256 string `json:"replacement_sha256"`
	PredecessorSHA256 string `json:"predecessor_sha256"`
	CoveredToken      string `json:"covered_token"`
}

func deletionIntentSHA(j SessionDeletion) string {
	j.Phase = DeletionPrepared
	j.RestorationSHA256 = ""
	j.RestorationToken = ""
	return deletionChecksum(j)
}

// ValidateRetentionRestoration requires durable deletion authority on every replay.
func (s *Store) ValidateRetentionRestoration(reg archive.SessionRegistration, p PendingPublication) error {
	if p.Commit == nil || p.Commit.Retention == nil {
		return nil
	}
	r := p.Commit.Retention
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found || j.Reason != RemovalReasonRetention || (j.Phase != DeletionRestoring && j.Phase != DeletionRestored) || j.Owner != r.OwnerSHA256 || r.ReplacementSHA256 != p.Commit.MetadataSHA256 || deletionIntentSHA(j) != r.DeletionSHA256 || j.MetadataSHA256 != r.PredecessorSHA256 || j.RestorationSHA256 != p.Commit.MetadataSHA256 || j.RestorationToken != r.CoveredToken || r.CoveredToken != p.RequestToken {
		return ErrAdmissionStageRecovery
	}
	return p.ValidatePublication()
}

func (s *Store) saveDeletionAtRequest(reg archive.SessionRegistration, j SessionDeletion, expected string) error {
	j.Checksum = deletionChecksum(j)
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return s.writeDeletionFileChecked(reg.ArchiveSessionID, raw, func() error {
		req, found, err := s.LoadRequest(reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		if found && req.Token != expected || !found && expected != "" {
			return ErrDeletionWorkChanged
		}
		return nil
	})
}

func canonicalDeletion(raw []byte, j SessionDeletion) bool {
	canonical, err := json.Marshal(j)
	return err == nil && bytes.Equal(raw, canonical)
}
