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
	if !info.Mode().IsRegular() || info.Size() > deletionControlLimit {
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
	out, err = io.ReadAll(io.LimitReader(h, deletionControlLimit+1))
	if err != nil {
		return nil, err
	}
	after, err := dir.Lstat(name)
	if err != nil || int64(len(out)) > deletionControlLimit || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, ErrAdmissionStageRecovery
	}
	return out, nil
}
func (s *Store) writeDeletionFile(id string, raw []byte) error {
	return s.writeDeletionFileChecked(id, raw, nil)
}
func (s *Store) writeDeletionFileChecked(id string, raw []byte, check func() error) (err error) {
	if int64(len(raw)) > deletionControlLimit {
		return ErrAdmissionStageCapacity
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
	rooted := *s
	rooted.quotaRoot = root
	if err = rooted.checkDeletionWriteCapacity(int64(len(raw))); err != nil {
		return err
	}
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
	if err = s.commitDeletionFile(dir, id, temp, check); err != nil {
		return err
	}
	h, err = dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(h.Sync(), h.Close())
}

func (s *Store) commitDeletionFile(dir *os.Root, id, temp string, check func() error) error {
	if check == nil {
		return dir.Rename(temp, id+".json")
	}
	unlock, err := s.lockRequest(id)
	if err != nil {
		return err
	}
	defer unlock()
	if err = check(); err != nil {
		return err
	}
	return dir.Rename(temp, id+".json")
}
