package state

import (
	"errors"
	"os"
	"path/filepath"
)

// deletionControlAllowance is reserved inside the existing pool, once for the
// whole store. It covers a bounded journal's old/new atomic coexistence, never
// extra capacity outside AdmissionStageQuota.
const deletionControlAllowance int64 = 64 << 10
const deletionControlLimit int64 = deletionControlAllowance / 2

// deletionControlUsage counts every regular control, including retained,
// restored, malformed and orphan temporary files, without reading their bodies.
func (s *Store) deletionControlUsage() (used int64, err error) {
	root := s.quotaRoot
	if root == nil {
		root, err = os.OpenRoot(s.home)
		if err != nil {
			return 0, err
		}
		defer func() { err = errors.Join(err, root.Close()) }()
	}
	rooted := *s
	rooted.quotaRoot = root
	directory := filepath.Join(s.home, "session-deletions")
	info, err := rooted.quotaLstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, ErrAdmissionStageRecovery
	}
	entries, err := rooted.quotaReadDir(directory)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		info, err := rooted.quotaLstat(filepath.Join(directory, entry.Name()))
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, ErrAdmissionStageRecovery
		}
		if info.Size() < 0 || info.Size() > AdmissionStageQuota-used {
			return 0, ErrAdmissionStageCapacity
		}
		used += info.Size()
	}
	return used, nil
}

func deletionControlCharge(actual int64) int64 {
	if actual < deletionControlAllowance {
		return deletionControlAllowance
	}
	return actual
}

// checkDeletionWriteCapacity runs under temporary-quota. Existing controls
// remain charged during the atomic write, including the destination's old copy.
func (s *Store) checkDeletionWriteCapacity(bytes int64) error {
	actual, err := s.deletionControlUsage()
	if err != nil {
		return err
	}
	used, err := s.admissionStageUsage()
	if err != nil {
		return err
	}
	ordinary := used - deletionControlCharge(actual)
	if bytes > AdmissionStageQuota-actual || deletionControlCharge(actual+bytes) > AdmissionStageQuota-ordinary {
		return ErrAdmissionStageCapacity
	}
	return nil
}
