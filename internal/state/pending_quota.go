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

func (s *Store) savePendingQuota(id string, p PendingPublication, required bool) error {
	if !required {
		for _, dir := range []string{admissionStageDir, temporaryReservationDir, temporaryScratchDir, publicationEvidenceDir} {
			present, e := s.quotaDirectoryHasEntries(filepath.Join(s.home, dir))
			if e != nil {
				return e
			}
			if present {
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
