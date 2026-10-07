package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

const quotaReceiptLimit int64 = 4096

type pendingQuotaReceipt struct {
	SessionID    string    `json:"session_id"`
	StageDigest  string    `json:"stage_digest"`
	FrozenSHA    string    `json:"frozen_sha"`
	PendingSHA   string    `json:"pending_sha"`
	Identity     string    `json:"identity"`
	EncodedBytes int64     `json:"encoded_bytes"`
	ModifiedAt   time.Time `json:"modified_at"`
	Checksum     string    `json:"checksum"`
	Version      int       `json:"version"`
}

func validQuotaStage(m AdmissionStage, id string) bool {
	return m.Version == 1 && m.Reservation.ArchiveSessionID == id && m.Reservation.AdmissionStage == "" && m.Reservation.Validate() == nil && m.Bytes > 0 && m.Bytes <= AdmissionStageCompressedLimit && len(m.SHA256) == 64 && m.ReservedBytes > 0 && m.ReservedBytes <= AdmissionStageQuota
}

func (s *Store) heldQuotaStage(id, digest string) (m AdmissionStage, allowance int64, ok bool) {
	reg, found, err := s.quotaRegistration(id)
	if err != nil || !found || reg.AdmissionStage != digest || digest == "" {
		return
	}
	released, err := s.quotaStageReleased(reg)
	if err != nil || released {
		return
	}
	path, err := s.quotaStagePath(id, ".json")
	if err != nil {
		return
	}
	raw, err := s.quotaReadFile(path, stageManifestLimit)
	if err != nil || stageDigest(raw) != digest || json.Unmarshal(raw, &m) != nil || !validQuotaStage(m, id) || CheckAdmissionStageOwnership(reg, m) != nil {
		return
	}
	object, _ := s.quotaStagePath(id, ".source.gz")
	info, err := s.quotaLstat(object)
	if err != nil || !info.Mode().IsRegular() || info.Size() != m.Bytes {
		return
	}
	allowance = max(0, m.ReservedBytes-2*m.Bytes-2*int64(len(raw)))
	return m, allowance, true
}

func (s *Store) quotaReceiptPath(id string) string {
	return filepath.Join(s.home, "pending", id+".quota")
}

func quotaReceiptDigest(r pendingQuotaReceipt) string {
	r.Checksum = ""
	raw, _ := json.Marshal(r)
	return stageDigest(raw)
}

// A receipt is quota-only. Stat-matching same-size corruption cannot increase
// the physical quota and never establishes content or publication authority.
func (s *Store) existingPendingQuotaCredit(id string, pendingInfo os.FileInfo) (receiptBytes, credit int64) {
	path := s.quotaReceiptPath(id)
	info, err := s.quotaLstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > AdmissionStageQuota/2 {
		return AdmissionStageQuota + 1, 0
	}
	receiptBytes = 2 * info.Size()
	raw, err := s.quotaReadFile(path, quotaReceiptLimit)
	var r pendingQuotaReceipt
	if err != nil || json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.SessionID != id || r.Checksum != quotaReceiptDigest(r) || len(r.PendingSHA) != 64 || r.EncodedBytes != pendingInfo.Size() || !r.ModifiedAt.Equal(pendingInfo.ModTime()) || r.Identity != pendingQuotaIdentity(pendingInfo) {
		return
	}
	m, allowance, ok := s.heldQuotaStage(id, r.StageDigest)
	if !ok || r.FrozenSHA != m.SHA256 {
		return
	}
	credit = min(2*pendingInfo.Size()+receiptBytes, allowance)
	return
}

func (s *Store) writeQuotaReceipt(id string, p PendingPublication, encoded []byte) error {
	info, err := s.quotaLstat(s.pendingPath(id))
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(encoded)) {
		return ErrAdmissionStageRecovery
	}
	r := pendingQuotaReceipt{SessionID: id, StageDigest: p.AdmissionStage, FrozenSHA: p.SourceSHA256, PendingSHA: stageDigest(encoded), Identity: pendingQuotaIdentity(info), EncodedBytes: info.Size(), ModifiedAt: info.ModTime(), Version: 1}
	r.Checksum = quotaReceiptDigest(r)
	return local.WriteCompact(s.quotaReceiptPath(id), r)
}

// Targeted reconciliation repairs only this pending's exact frozen stage. The
// caller owns quota serialization; unknown/corrupt receipts receive no credit.
func (s *Store) reconcileQuotaReceipt(id string, info os.FileInfo) error {
	_, credit := s.existingPendingQuotaCredit(id, info)
	if credit > 0 {
		return nil
	}
	if s.onQuotaBodyRead != nil {
		s.onQuotaBodyRead("pending")
	}
	raw, err := s.quotaReadFile(s.pendingPath(id), AdmissionStageQuota/2)
	if err != nil {
		return err
	}
	var p PendingPublication
	if json.Unmarshal(raw, &p) != nil {
		return ErrAdmissionStageRecovery
	}
	if p.Commit != nil {
		if err = p.ValidatePublication(); err != nil {
			return err
		}
	}
	available := s.coveredPendingCredit(id, p, AdmissionStageQuota)
	if available <= 0 {
		return nil
	}
	if 2*int64(len(raw))+2*quotaReceiptLimit > available {
		return ErrAdmissionStageCapacity
	}
	return s.writeQuotaReceipt(id, p, raw)
}

func (s *Store) removeQuotaReceipt(id string) error {
	err := os.Remove(s.quotaReceiptPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
