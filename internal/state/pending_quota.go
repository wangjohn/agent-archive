package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"time"
)

// SavePendingWithTemporaryReservation writes pending publication alongside
// scratch-only privacy capacity. Owner, session and canonical store must match;
// the pending atomic copy is independently bounded by the shared quota gate.
func (s *Store) SavePendingWithTemporaryReservation(r *TemporaryReservation, id string, p PendingPublication) error {
	if r == nil {
		return ErrAdmissionStageRecovery
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	canonical, err := filepath.EvalSymlinks(s.home)
	if err != nil || canonical != r.store.home || r.closed || r.err != nil || r.manifest.Owner != PublicationPrivacy || r.manifest.Key != id {
		return ErrAdmissionStageRecovery
	}
	if r.bytes == 0 {
		if r.manifest.Charged != 0 {
			return ErrAdmissionStageRecovery
		}
		if _, e := r.root.Lstat(r.manifest.Root); !errors.Is(e, os.ErrNotExist) {
			return ErrAdmissionStageRecovery
		}
		if _, e := r.root.Lstat(filepath.Join(temporaryReservationDir, r.manifest.Token+".json")); !errors.Is(e, os.ErrNotExist) {
			return ErrAdmissionStageRecovery
		}
	}
	// Keep SavePending's structural validation without accidentally taking the
	// quota lock twice: the force flag is local to this call, not stored policy.
	if !safeFileComponent(id) || p.SourceKey == "" || p.MetadataKey == "" || p.SourceSHA256 == "" || len(p.MetadataBytes) == 0 || (len(p.SourceBytes) == 0 && (!p.MetadataOnly || p.SourceSize <= 0)) {
		return ErrAdmissionStageRecovery
	}
	if p.Commit != nil {
		if err = p.ValidatePublication(); err != nil {
			return err
		}
	}
	return s.savePendingQuota(id, p, true)
}

func (s *Store) savePendingQuota(id string, p PendingPublication, required bool) error {
	if !required {
		for _, dir := range []string{admissionStageDir, temporaryReservationDir, publicationEvidenceDir} {
			entries, e := os.ReadDir(filepath.Join(s.home, dir))
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			if len(entries) > 0 {
				required = true
				break
			}
		}
	}
	if !required {
		return local.WriteCompact(s.pendingPath(id), p)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > AdmissionStageQuota/2 {
		return ErrAdmissionStageCapacity
	}
	unlock, err := s.namedLockWait("temporary-quota", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	available := s.coveredPendingCredit(id, p, AdmissionStageQuota)
	if old, e := os.Lstat(s.pendingPath(id)); e == nil && available > 0 {
		if err = s.reconcileQuotaReceipt(id, old); err != nil {
			return err
		}
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	used, err := s.admissionStageUsage()
	if err != nil {
		return err
	}
	charge := 2 * int64(len(encoded))
	if available > 0 {
		charge += 2 * quotaReceiptLimit
	}
	if old, e := os.Lstat(s.pendingPath(id)); e == nil {
		_, oldCredit := s.existingPendingQuotaCredit(id, old)
		available = max(0, available-oldCredit)
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if charge-min(charge, available) > AdmissionStageQuota-used {
		return ErrAdmissionStageCapacity
	}
	if err = local.WriteBytes(s.pendingPath(id), encoded); err != nil {
		return err
	}
	if s.onQuotaReceipt != nil {
		if err = s.onQuotaReceipt(); err != nil {
			return err
		}
	}
	if available > 0 {
		return s.writeQuotaReceipt(id, p, encoded)
	}
	return s.removeQuotaReceipt(id)
}

// coveredPendingCredit consumes only an admitted stage's verified future-copy
// allowance. Only an exact source or validated immutable-stage privacy receipt
// is covered; unknown, released or malformed evidence gets no credit.
func (s *Store) coveredPendingCredit(id string, p PendingPublication, charge int64) int64 {
	reg, found, err := s.LoadRegistration(id)
	if err != nil || !found || reg.AdmissionStage == "" || p.AdmissionStage != reg.AdmissionStage {
		return 0
	}
	released, err := s.AdmissionStageReleased(reg)
	if err != nil || released {
		return 0
	}
	if s.onQuotaBodyRead != nil {
		s.onQuotaBodyRead("stage")
	}
	m, bundle, err := s.ReadAdmissionStage(id, reg.AdmissionStage)
	if err != nil || CheckAdmissionStageOwnership(reg, m) != nil {
		return 0
	}
	if p.Commit != nil && p.Commit.Purpose == PublicationPrivacyRewrite {
		if p.CheckAdmissionStageTransform(reg, m, bundle) != nil {
			return 0
		}
	} else {
		if p.SourceSHA256 != m.SHA256 || int64(len(p.SourceBytes)) != m.Bytes || stageDigest(p.SourceBytes) != m.SHA256 {
			return 0
		}
		expected, e := json.Marshal(bundle)
		actual, a := json.Marshal(p.Bundle)
		if e != nil || a != nil || !bytes.Equal(expected, actual) {
			return 0
		}
	}
	manifest, _ := s.stagePath(id, ".json")
	info, err := os.Lstat(manifest)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	allowance := m.ReservedBytes - 2*m.Bytes - 2*info.Size()
	return max(0, min(charge, allowance))
}
