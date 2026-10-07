package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// prepareLocalRemoval records explicit local whole-session authority before any
// admitted bytes disappear. Remote callers must have completed their own deletion.
func (s *Store) prepareLocalRemoval(reg archive.SessionRegistration) error {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found || j.Phase == "restoring" || j.Phase == "restored" {
		j, err = s.PrepareSessionDeletion(reg, RemovalReasonUndo, nil, time.Now())
		if err != nil {
			return err
		}
	}
	if j.Phase == "prepared" {
		if err = s.AdvanceSessionDeletion(reg, "deleting"); err != nil {
			return err
		}
		j.Phase = "deleting"
	}
	if j.Phase == "deleting" {
		if j.MetadataSHA256 != "" {
			return ErrAdmissionStageRecovery
		}
		if err = s.AdvanceSessionDeletion(reg, "absent"); err != nil {
			return err
		}
		j.Phase = "absent"
	}
	if j.Phase == "absent" {
		return s.AdvanceSessionDeletion(reg, "cleaned")
	}
	return nil
}

func (s *Store) removeDurableSessionEvidence(reg archive.SessionRegistration) (err error) {
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found || j.Phase != "cleaned" {
		return ErrAdmissionStageRecovery
	}
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	unlock, err := temporaryQuotaLock(root, time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	// Do not follow a corrupt stage directory or remove another reservation.
	if _, err = s.stagePath(reg.ArchiveSessionID, ".json"); err != nil {
		return err
	}
	if _, err = s.evidencePath(reg.ArchiveSessionID); err != nil {
		return err
	}
	if err = removeOwnedStageFiles(root, reg.ArchiveSessionID); err != nil {
		return err
	}
	if err = removeOwnedOriginalFiles(root, reg.ArchiveSessionID); err != nil {
		return err
	}
	if err = s.removeSessionScratch(root, reg.ArchiveSessionID); err != nil {
		return err
	}
	if err = s.removeQuotaReceipt(reg.ArchiveSessionID); err != nil {
		return err
	}
	h, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(h.Sync(), h.Close())
}

// removeSessionScratch runs under the existing shared quota lock, after explicit
// whole-session removal. Only checksum-free accounting receipts with a fully
// validated session/owner/root tuple authorize their own scratch cleanup.
func (s *Store) removeSessionScratch(root *os.Root, id string) error {
	info, err := root.Lstat(temporaryReservationDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrAdmissionStageRecovery
	}
	dir, err := openRemovalDirectory(root, temporaryReservationDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	entries, err := removalDirectoryEntries(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		raw, err := readRemovalReceipt(dir, entry.Name())
		if err != nil {
			return err
		}
		m, err := decodeRemovalReceipt(raw, entry.Name())
		if err != nil {
			return err
		}
		if m.Key != id {
			continue
		}
		if err := removeOwnedScratch(root, m.Token); err != nil {
			return err
		}

		if err = dir.Remove(entry.Name()); err != nil {
			return err
		}
	}
	h, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(h.Sync(), h.Close())
}

func decodeRemovalReceipt(raw []byte, name string) (temporaryManifest, error) {
	var m temporaryManifest
	if json.Unmarshal(raw, &m) != nil || m.Version != 1 || !safeFileComponent(m.Key) || !safeFileComponent(m.Token) || name != m.Token+".json" || m.Root != filepath.Join(temporaryScratchDir, m.Token) || m.Charged < temporaryControlBytes || m.Charged > AdmissionStageQuota || (m.Owner != CursorAdmission && m.Owner != PublicationPrivacy) {
		return m, ErrAdmissionStageRecovery
	}
	return m, nil
}

func readRemovalReceipt(root *os.Root, name string) (raw []byte, err error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > temporaryControlBytes {
		return nil, ErrAdmissionStageRecovery
	}
	h, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, h.Close()) }()
	opened, err := h.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrAdmissionStageRecovery
	}
	raw, err = io.ReadAll(io.LimitReader(h, temporaryControlBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := root.Lstat(name)
	if err != nil || int64(len(raw)) > temporaryControlBytes || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, ErrAdmissionStageRecovery
	}
	return raw, nil
}

func removeOwnedStageFiles(root *os.Root, id string) error {
	var err error
	dir, e := openRemovalDirectory(root, admissionStageDir)
	if e == nil {
		entries, e := removalDirectoryEntries(dir)
		if e != nil {
			_ = dir.Close()
			return e
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), id+".") {
				continue
			}
			info, e := dir.Lstat(entry.Name())
			if e != nil || !info.Mode().IsRegular() {
				_ = dir.Close()
				return ErrAdmissionStageRecovery
			}
			if e = dir.Remove(entry.Name()); e != nil {
				_ = dir.Close()
				return e
			}
		}
		h, e := dir.Open(".")
		if e == nil {
			e = errors.Join(h.Sync(), h.Close())
		}
		err = errors.Join(e, dir.Close())
		if err != nil {
			return err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}

	return nil
}

func removeOwnedOriginalFiles(root *os.Root, id string) error {
	// An explicit removal covers confined original journals, including orphan
	// atomic files. Corrupt subdirectories/symlinks stay charged and actionable.
	parent, e := openRemovalDirectory(root, publicationEvidenceDir)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer func() { _ = parent.Close() }()
	evidence, e := openRemovalDirectory(parent, id)
	if e == nil {
		entries, e := removalDirectoryEntries(evidence)
		if e != nil {
			_ = evidence.Close()
			return e
		}
		for _, entry := range entries {
			info, e := evidence.Lstat(entry.Name())
			if e != nil || !info.Mode().IsRegular() {
				_ = evidence.Close()
				return ErrAdmissionStageRecovery
			}
			if e = evidence.Remove(entry.Name()); e != nil {
				_ = evidence.Close()
				return e
			}
		}
		h, e := evidence.Open(".")
		if e == nil {
			e = errors.Join(h.Sync(), h.Close())
		}
		e = errors.Join(e, evidence.Close())
		if e != nil {
			return e
		}
		if e = parent.Remove(id); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}

	return nil
}

func openRemovalDirectory(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrAdmissionStageRecovery
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(ErrAdmissionStageRecovery, err, dir.Close())
	}
	return dir, nil
}

func removalDirectoryEntries(dir *os.Root) ([]os.DirEntry, error) {
	rooted := Store{home: ".", quotaRoot: dir}
	return rooted.quotaReadDir(".")
}

func removeOwnedScratch(root *os.Root, token string) (err error) {
	parent, err := openRemovalDirectory(root, temporaryScratchDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	if err = parent.RemoveAll(token); err != nil {
		return err
	}
	h, err := parent.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(h.Sync(), h.Close())
}
