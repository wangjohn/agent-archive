package state

import (
	"errors"
	"io"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func (s *Store) openDeletionDirectory(create bool) (_ *os.Root, err error) {
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, e := root.Lstat("session-deletions")
	if errors.Is(e, os.ErrNotExist) && create {
		if e = root.Mkdir("session-deletions", 0700); e != nil {
			return nil, e
		}
		info, e = root.Lstat("session-deletions")
	}
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrAdmissionStageRecovery
	}
	dir, err := root.OpenRoot("session-deletions")
	if err != nil {
		return nil, err
	}
	h, err := dir.Open(".")
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	opened, e := h.Stat()
	e = errors.Join(e, h.Close())
	if e != nil || !os.SameFile(info, opened) {
		_ = dir.Close()
		return nil, ErrAdmissionStageRecovery
	}
	if create {
		h, e = root.Open(".")
		if e == nil {
			e = errors.Join(h.Sync(), h.Close())
		}
		if e != nil {
			_ = dir.Close()
			return nil, e
		}
	}
	return dir, nil
}
func (s *Store) readDeletionFile(id string) (out []byte, err error) {
	dir, err := s.openDeletionDirectory(false)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	name := id + ".json"
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > stageManifestLimit {
		return nil, ErrAdmissionStageRecovery
	}
	h, err := dir.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, h.Close()) }()
	opened, err := h.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrAdmissionStageRecovery
	}
	out, err = io.ReadAll(io.LimitReader(h, stageManifestLimit+1))
	if err != nil {
		return nil, err
	}
	after, err := dir.Lstat(name)
	if err != nil || int64(len(out)) > stageManifestLimit || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, ErrAdmissionStageRecovery
	}
	return out, nil
}
func (s *Store) writeDeletionFile(id string, raw []byte) (err error) {
	if int64(len(raw)) > stageManifestLimit {
		return ErrAdmissionStageCapacity
	}
	unlock, err := s.namedLockWait("session-deletions.lock", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := s.openDeletionDirectory(true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	token, err := local.ID()
	if err != nil {
		return err
	}
	temp := id + "-" + token + ".tmp"
	h, err := dir.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		e := dir.Remove(temp)
		if !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	n, e := h.Write(raw)
	if e == nil && n != len(raw) {
		e = io.ErrShortWrite
	}
	err = errors.Join(e, h.Sync(), h.Close())
	if err != nil {
		return err
	}
	if err = dir.Rename(temp, id+".json"); err != nil {
		return err
	}
	h, err = dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(h.Sync(), h.Close())
}
