package state

import (
	"errors"
	"os"
	"path/filepath"
)

const publicationEvidenceDir = "publication-evidence"

const publicationEvidenceControl int64 = 64 << 10

func (s *Store) publicationEvidenceUsage() (int64, error) {
	base := filepath.Join(s.home, publicationEvidenceDir)
	if info, err := s.quotaLstat(base); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return 0, ErrAdmissionStageRecovery
	}
	entries, err := s.quotaReadDir(base)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var used int64
	remaining := 65536
	for _, entry := range entries {
		if !safeFileComponent(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return 0, ErrAdmissionStageRecovery
		}
		files, err := s.quotaReadDir(filepath.Join(base, entry.Name()))
		if err != nil {
			return 0, err
		}
		if len(files) > remaining {
			return 0, ErrAdmissionStageRecovery
		}
		remaining -= len(files)
		if len(files) == 0 {
			continue
		}
		if publicationEvidenceControl > AdmissionStageQuota-used {
			return 0, ErrAdmissionStageCapacity
		}
		used += publicationEvidenceControl
		for _, file := range files {
			info, err := s.quotaLstat(filepath.Join(base, entry.Name(), file.Name()))
			if err != nil || !info.Mode().IsRegular() {
				return 0, ErrAdmissionStageRecovery
			}
			if info.Size() < 0 || info.Size() > (AdmissionStageQuota-used)/2 {
				return 0, ErrAdmissionStageCapacity
			}
			used += 2 * info.Size()
		}
	}
	return used, nil
}
