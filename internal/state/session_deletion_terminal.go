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
	if !found || j.Phase != "cleaned" {
		return ErrAdmissionStageRecovery
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
	raw, err := s.readDeletionFile(id)
	if err != nil {
		return false
	}
	var j SessionDeletion
	return s.localDeletionRecordsGone(id) && json.Unmarshal(raw, &j) == nil && j.Version == 1 && j.LocalRemoved && j.Phase == "cleaned" && !j.At.IsZero() && validPublicationDigest(j.Owner) && j.Checksum == deletionChecksum(j) && (j.Reason == RemovalReasonRetention || j.Reason == RemovalReasonUndo)
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
		if strings.HasSuffix(entry.Name(), ".json") {
			id := strings.TrimSuffix(entry.Name(), ".json")
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
	for _, path := range []string{"registrations/" + id + ".json", "requests/" + id + ".json", "published/" + id + ".json", "pending/" + id + ".json", "superseded/" + id + ".json", filepath.Join(publicationEvidenceDir, id)} {
		if _, err := root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	rooted := *s
	rooted.quotaRoot = root
	entries, err := rooted.quotaReadDir(filepath.Join(s.home, "sessions", id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	if len(entries) != 0 {
		return false
	}
	entries, err = rooted.quotaReadDir(filepath.Join(s.home, admissionStageDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), id+".") {
			return false
		}
	}
	return true
}
