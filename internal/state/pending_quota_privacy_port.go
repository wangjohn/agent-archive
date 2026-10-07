package state

import (
	"errors"
	"os"
	"path/filepath"
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
	// A canonical pathname can be reused for a different home inode after the
	// reservation was opened. It must still identify the held state authority
	// before this consumer enters the ordinary single-owner pending writer.
	held, err := r.root.Stat(".")
	if err != nil {
		return ErrAdmissionStageRecovery
	}
	current, err := os.Stat(canonical)
	if err != nil || !os.SameFile(held, current) {
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
