package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

const publicationEvidenceLimit int64 = 512 << 20

func (s *Store) evidencePath(id string) (string, error) {
	if !safeFileComponent(id) {
		return "", ErrAdmissionStageRecovery
	}
	dir := filepath.Join(s.home, publicationEvidenceDir)
	for _, path := range []string{dir, filepath.Join(dir, id)} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", ErrAdmissionStageRecovery
		}
	}
	return filepath.Join(dir, id, "journal.json"), nil
}

// SavePublicationEvidence preserves bounded caller-validated original evidence
// in a private per-session journal. It grants no content authority. Shared quota
// covers the old/new atomic envelope alongside pending, stages and scratch;
// maintenance owns the typed schema and successor/replacement validation.
func (s *Store) SavePublicationEvidence(id string, encoded []byte) error {
	if len(encoded) == 0 || int64(len(encoded)) > publicationEvidenceLimit {
		return ErrAdmissionStageCapacity
	}
	path, err := s.evidencePath(id)
	if err != nil {
		return err
	}
	unlock, err := s.namedLockWait("temporary-quota", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	used, err := s.admissionStageUsage()
	if err != nil {
		return err
	}
	oldBytes := int64(0)
	control := publicationEvidenceControl
	if info, e := os.Lstat(path); e == nil {
		if !info.Mode().IsRegular() || info.Size() > publicationEvidenceLimit {
			return ErrAdmissionStageRecovery
		}
		oldBytes = 2 * info.Size()
		control = 0
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	additional := max(0, 2*int64(len(encoded))-oldBytes) + control
	if additional > AdmissionStageQuota-used {
		return ErrAdmissionStageCapacity
	}
	return s.writeEvidenceAtomic(id, encoded)
}

func (s *Store) writeEvidenceAtomic(id string, encoded []byte) (err error) {
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	relative := filepath.Join(publicationEvidenceDir, id)
	if err = root.MkdirAll(relative, 0700); err != nil {
		return err
	}
	dir, err := root.OpenRoot(relative)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	token, err := local.ID()
	if err != nil {
		return err
	}
	temp := "journal-" + token + ".tmp"
	f, err := dir.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		e := dir.Remove(temp)
		if !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	n, writeErr := f.Write(encoded)
	if n != len(encoded) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = dir.Rename(temp, "journal.json"); err != nil {
		return err
	}
	handle, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}

// ReadPublicationEvidence reads only the named bounded private journal. Missing
// or malformed bytes remain the maintenance owner's responsibility to classify.
func (s *Store) ReadPublicationEvidence(id string) (out []byte, err error) {
	_, err = s.evidencePath(id)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	dir, err := root.OpenRoot(filepath.Join(publicationEvidenceDir, id))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	before, err := dir.Lstat("journal.json")
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > publicationEvidenceLimit {
		return nil, ErrAdmissionStageRecovery
	}
	f, err := dir.Open("journal.json")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrAdmissionStageRecovery
	}
	raw, err := io.ReadAll(io.LimitReader(f, publicationEvidenceLimit+1))
	if err != nil {
		return nil, err
	}
	after, err := dir.Lstat("journal.json")
	if err != nil || int64(len(raw)) > publicationEvidenceLimit || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, ErrAdmissionStageRecovery
	}
	return raw, nil
}

// RemovePublicationEvidence removes only the named private journal after the
// maintenance owner validates exact local successor commitment. Unknown files
// remain charged recovery evidence; this primitive never removes native input.
func (s *Store) RemovePublicationEvidence(id string) (err error) {
	_, err = s.evidencePath(id)
	if err != nil {
		return err
	}
	unlock, err := s.namedLockWait("temporary-quota", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	homeRoot, e := os.OpenRoot(s.home)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, homeRoot.Close()) }()
	root, err := homeRoot.OpenRoot(filepath.Join(publicationEvidenceDir, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if err = root.Remove("journal.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
