package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// completeLocalDeletion runs only after the original owner's complete local
// forget/index commits. Lost-owner recovery never fabricates this authority.
func (s *Store) completeLocalDeletion(reg archive.SessionRegistration) error {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found || j.Phase != DeletionCleaned {
		return ErrAdmissionStageRecovery
	}
	if err := s.syncLocalDeletion(); err != nil {
		return err
	}
	if !s.localDeletionRecordsGone(reg.ArchiveSessionID) {
		return ErrAdmissionStageRecovery
	}
	j.LocalRemoved = true
	return s.saveSessionDeletion(reg, j)
}

// terminalDeletion recognizes only finished, checksum-bound bookkeeping.
// It grants no publication, owner transition or native content permission.
func (s *Store) terminalDeletion(id string) bool {
	if !safeFileComponent(id) {
		return false
	}
	temporary, err := s.hasDeletionTemporary(id)
	if err != nil || temporary {
		return false
	}
	raw, err := s.readDeletionFile(id)
	if err != nil {
		return false
	}
	var j SessionDeletion
	return s.localDeletionRecordsGone(id) && json.Unmarshal(raw, &j) == nil && canonicalDeletion(raw, j) && j.Version == 1 && j.LocalRemoved && j.Phase == DeletionCleaned && !j.At.IsZero() && validPublicationDigest(j.Owner) && j.Checksum == deletionChecksum(j) && (j.Reason == RemovalReasonRetention || j.Reason == RemovalReasonUndo)
}

func (s *Store) deletionOrphanStems(directory string) (out []string, err error) {
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrAdmissionStageRecovery
	}
	rooted := *s
	rooted.quotaRoot = root
	entries, err := rooted.quotaReadDir(filepath.Join(s.home, directory))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if stem, temporary := strings.CutSuffix(entry.Name(), ".tmp"); directory == "session-deletions" && temporary {
			// IDs may contain dashes; atomic tokens are the final 32 hex digits.
			if len(stem) > 33 && stem[len(stem)-33] == '-' {
				id := stem[:len(stem)-33]
				if safeFileComponent(id) {
					out = append(out, id)
				}
			} else {
				return nil, ErrAdmissionStageRecovery
			}
		}
		if id, final := strings.CutSuffix(entry.Name(), ".json"); final {
			if safeFileComponent(id) {
				out = append(out, id)
			}
		}
	}
	return out, nil
}

func (s *Store) localDeletionRecordsGone(id string) bool {
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"registrations", "requests", "published", "pending", "superseded", "pending-scans", "scan-signatures", "refresh-skips", "listing-repairs", "sessions", admissionStageDir, publicationEvidenceDir, temporaryReservationDir, temporaryScratchDir} {
		dir, err := openRemovalDirectory(root, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false
		}
		if err := dir.Close(); err != nil {
			return false
		}
	}
	for _, path := range []string{"registrations/" + id + ".json", "requests/" + id + ".json", "published/" + id + ".json", "pending/" + id + ".json", "superseded/" + id + ".json", "pending/" + id + ".quota", "pending-scans/" + id + ".json", "scan-signatures/" + id + ".json", "refresh-skips/" + id + ".json", "listing-repairs/" + id + ".json", filepath.Join(publicationEvidenceDir, id)} {
		if _, err := root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	rooted := *s
	rooted.quotaRoot = root
	sessions, err := openRemovalDirectory(root, "sessions")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if sessions != nil {
		dir, err := openRemovalDirectory(sessions, id)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = sessions.Close()
			return false
		}
		if dir != nil {
			entries, err := removalDirectoryEntries(dir)
			err = errors.Join(err, dir.Close(), sessions.Close())
			if err != nil || len(entries) != 0 {
				return false
			}
		} else if err := sessions.Close(); err != nil {
			return false
		}
	}
	entries, err := rooted.quotaReadDir(filepath.Join(s.home, admissionStageDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for _, entry := range entries {
		if ownedStageFilename(id, entry.Name()) {
			return false
		}
	}
	if owed, err := sessionScratchOutstanding(root, id); err != nil || owed {
		return false
	}
	return true
}

func (s *Store) hasDeletionTemporary(id string) (bool, error) {
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	rooted := *s
	rooted.quotaRoot = root
	entries, err := rooted.quotaReadDir(filepath.Join(s.home, "session-deletions"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), id+"-") && strings.HasSuffix(entry.Name(), ".tmp") {
			return true, nil
		}
	}
	return false, nil
}

// Sync each directory whose owning unlink must survive before the terminal flag.
// A home sync alone does not durably commit nested directory changes.
func (s *Store) syncLocalDeletion() (err error) {
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	for _, name := range []string{"registrations", "requests", "published", "pending", "pending-scans", "scan-signatures", "superseded", "refresh-skips", "listing-repairs", "request-locks", "sessions", admissionStageDir, publicationEvidenceDir, temporaryReservationDir, temporaryScratchDir} {
		dir, e := openRemovalDirectory(root, name)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		h, e := dir.Open(".")
		if e == nil && s.onLocalDeletionSync != nil {
			e = s.onLocalDeletionSync(name)
		}
		if h != nil {
			if e == nil {
				e = h.Sync()
			}
			e = errors.Join(e, h.Close())
		}
		e = errors.Join(e, dir.Close())
		if e != nil {
			return e
		}
	}
	h, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(h.Sync(), h.Close())
}
