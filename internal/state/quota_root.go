package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func (s *Store) quotaRelative(path string) (string, error) {
	return filepath.Rel(s.home, path)
}

func (s *Store) quotaLstat(path string) (os.FileInfo, error) {
	if s.quotaRoot == nil {
		return os.Lstat(path)
	}
	relative, err := s.quotaRelative(path)
	if err != nil {
		return nil, err
	}
	return s.quotaRoot.Lstat(relative)
}

func (s *Store) quotaReadDir(path string) (entries []os.DirEntry, err error) {
	var directory *os.File
	if s.quotaRoot == nil {
		directory, err = os.Open(path)
	} else {
		relative, e := s.quotaRelative(path)
		if e != nil {
			return nil, e
		}
		directory, err = s.quotaRoot.Open(relative)
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	for {
		batch, e := directory.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return nil, e
		}
		if len(batch) > 65536-len(entries) {
			return nil, ErrAdmissionStageRecovery
		}
		entries = append(entries, batch...)
		if errors.Is(e, io.EOF) {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (s *Store) quotaReadFile(path string, limit int64) (body []byte, err error) {
	if s.quotaRoot == nil {
		return readStageFile(path, limit)
	}
	relative, err := s.quotaRelative(path)
	if err != nil {
		return nil, err
	}
	before, err := s.quotaRoot.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > limit {
		return nil, ErrAdmissionStageRecovery
	}
	f, err := s.quotaRoot.Open(relative)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, ErrAdmissionStageRecovery
	}
	body, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := s.quotaRoot.Lstat(relative)
	if err != nil || len(body) > int(limit) || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, ErrAdmissionStageRecovery
	}
	return body, nil
}

func (s *Store) quotaRegistration(id string) (reg archive.SessionRegistration, found bool, err error) {
	if !safeFileComponent(id) {
		return reg, false, ErrAdmissionStageRecovery
	}
	body, err := s.quotaReadFile(s.registrationPath(id), stageManifestLimit)
	if errors.Is(err, os.ErrNotExist) {
		return reg, false, nil
	}
	if err != nil {
		return reg, false, err
	}
	err = json.Unmarshal(body, &reg)
	return reg, err == nil, err
}

func (s *Store) quotaStagePath(id, suffix string) (string, error) {
	if !validStageID(id) {
		return "", ErrAdmissionStageRecovery
	}
	directory := filepath.Join(s.home, admissionStageDir)
	info, err := s.quotaLstat(directory)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", ErrAdmissionStageRecovery
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return filepath.Join(directory, id+suffix), nil
}

func (s *Store) quotaStageReleased(reg archive.SessionRegistration) (bool, error) {
	path, err := s.quotaStagePath(reg.ArchiveSessionID, ".released")
	if err != nil {
		return false, err
	}
	body, err := s.quotaReadFile(path, stageManifestLimit)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var release stageRelease
	if json.Unmarshal(body, &release) != nil || release.Digest != reg.AdmissionStage || len(release.SourceSHA256) != 64 || release.Checksum != stageReleaseChecksum(release) {
		return false, ErrAdmissionStageRecovery
	}
	return release.Complete, nil
}

func (r *TemporaryReservation) writeManifest(m temporaryManifest) (err error) {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if int64(len(body))*2 > temporaryControlBytes {
		return ErrAdmissionStageCapacity
	}
	directory, err := r.root.OpenRoot(temporaryReservationDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	name := m.Token + ".tmp"
	f, err := directory.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// An interrupted control remains visible and makes later accounting refuse.
	if n, e := f.Write(body); e != nil || n != len(body) {
		err = errors.Join(e, io.ErrShortWrite)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = directory.Rename(name, m.Token+".json"); err != nil {
		return err
	}
	held, err := directory.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(held.Sync(), held.Close())
}

func (s *Store) temporaryPhysicalUsage(path string, remaining *int) (used int64, err error) {
	relative, err := s.quotaRelative(path)
	if err != nil {
		return 0, err
	}
	var root *os.Root
	if s.quotaRoot == nil {
		root, err = os.OpenRoot(path)
	} else {
		root, err = s.quotaRoot.OpenRoot(relative)
	}
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	err = scanTemporaryPhysical(root, ".", 0, remaining, &used)
	return used, err
}

func scanTemporaryPhysical(root *os.Root, path string, depth int, remaining *int, used *int64) (err error) {
	if depth > 64 {
		return ErrAdmissionStageRecovery
	}
	directory, err := root.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	for {
		entries, e := directory.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, entry := range entries {
			*remaining--
			if *remaining < 0 {
				return ErrAdmissionStageRecovery
			}
			child := filepath.Join(path, entry.Name())
			info, e := root.Lstat(child)
			if e != nil {
				return e
			}
			if info.IsDir() {
				if e = scanTemporaryPhysical(root, child, depth+1, remaining, used); e != nil {
					return e
				}
			} else {
				if !info.Mode().IsRegular() || info.Size() < 0 {
					return ErrAdmissionStageRecovery
				}
				if info.Size() > AdmissionStageQuota-*used-temporaryControlBytes {
					return ErrAdmissionStageCapacity
				}
				*used += info.Size()
			}
		}
		if errors.Is(e, io.EOF) {
			return nil
		}
	}
}
