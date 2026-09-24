package collector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// SupersededSource is a source object that was once the current snapshot
// for a session and no longer is. It stays eligible for deletion, not
// deleted immediately, so the spec's grace period can protect a reader
// mid-download of what was, until a moment ago, the current pointer.
type SupersededSource struct {
	Key          string    `json:"key"`
	SupersededAt time.Time `json:"superseded_at"`
}

func (s *LocalStore) supersededPath(archiveSessionID string) string {
	return filepath.Join(s.home, "superseded", archiveSessionID+".json")
}

// RecordSuperseded appends key to a session's superseded-source ledger, so
// a later retention sweep can delete it once its grace period elapses. A
// session republished more than once before a sweep ever runs must not
// lose track of an earlier supersession, so each key is tracked
// individually rather than only the most recent one. If key is already
// recorded (content reverted to an earlier snapshot and was then
// superseded again), the entry moves to the end of the ledger with a fresh
// SupersededAt: append order is the supersession order retention relies
// on to identify the immediate predecessor of the current snapshot, so
// the most recently superseded key must always be last.
func (s *LocalStore) RecordSuperseded(archiveSessionID, key string, at time.Time) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	existing, err := s.LoadSuperseded(archiveSessionID)
	if err != nil {
		return err
	}
	out := make([]SupersededSource, 0, len(existing)+1)
	for _, e := range existing {
		if e.Key != key {
			out = append(out, e)
		}
	}
	out = append(out, SupersededSource{Key: key, SupersededAt: at})
	return local.Write(s.supersededPath(archiveSessionID), out)
}

// LoadSuperseded returns a session's superseded-source ledger.
func (s *LocalStore) LoadSuperseded(archiveSessionID string) ([]SupersededSource, error) {
	var out []SupersededSource
	err := local.Read(s.supersededPath(archiveSessionID), &out)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read superseded sources %q: %w", archiveSessionID, err)
	}
	return out, nil
}

// RemoveSuperseded drops one entry from a session's ledger, after its
// object has actually been deleted from storage.
func (s *LocalStore) RemoveSuperseded(archiveSessionID, key string) error {
	existing, err := s.LoadSuperseded(archiveSessionID)
	if err != nil {
		return err
	}
	out := existing[:0]
	for _, e := range existing {
		if e.Key != key {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		err := os.Remove(s.supersededPath(archiveSessionID))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove superseded ledger %q: %w", archiveSessionID, err)
		}
		return nil
	}
	return local.Write(s.supersededPath(archiveSessionID), out)
}

// ForgetIdleSession forgets a session under its request lock, the same lock a
// hook holds while it writes a request. Retention decides a session has
// expired from a snapshot taken earlier in the sweep; a hook can write a
// request for it after that snapshot. When deferForWork is set, the pending
// work is checked again under the lock, and a session that now has a request
// or a pending publication is kept (forgotten reports false) so the collector
// publishes that evidence. A session the collector no longer publishes is
// forgotten regardless: its work would never be done.
//
// A non-nil removal is recorded (see RecordRemoval) under the same lock,
// after that recheck and before anything is forgotten: a session kept alive
// gets no record, and a record that cannot be written leaves the session
// registered, so the caller's next attempt retries both.
func (s *LocalStore) ForgetIdleSession(archiveSessionID, nativeSessionID string, deferForWork bool, removal *RemovalRecord) (forgotten bool, err error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	unlock, err := s.lockRequest(archiveSessionID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if deferForWork {
		_, requested, err := s.loadRequest(archiveSessionID)
		if err != nil {
			return false, err
		}
		pending, err := s.HasPending(archiveSessionID)
		if err != nil {
			return false, err
		}
		if requested || pending {
			return false, nil
		}
	}
	if removal != nil {
		if err := s.RecordRemoval(removal.Harness, nativeSessionID, removal.Reason, removal.At); err != nil {
			return false, err
		}
	}
	if err := s.ForgetSession(archiveSessionID, nativeSessionID); err != nil {
		return false, err
	}
	return true, nil
}

// SessionDir is the per-session directory under the collector-owned
// sessions/ tree where other packages keep session-scoped evidence (the CLI's
// read-back verification record, for one). ForgetSession clears it.
func (s *LocalStore) SessionDir(archiveSessionID string) string {
	return filepath.Join(s.home, "sessions", archiveSessionID)
}

// ForgetSession removes every local record of a session: its registration,
// request, request and subagent-candidate locks, published-bundle cache, pending publication and
// scan markers, superseded-source ledger, per-session evidence directory,
// and native-session index entry. A caller uses this only after successfully
// deleting that session's metadata and every source object from storage
// (whole-session retention); it never touches storage itself. Retention calls
// it through ForgetIdleSession, under the request lock.
//
// Every record, the native-session index included, is removed before the
// request lock file. The lock is a flock on that file's inode, so once the
// file is unlinked a waiting hook can lock a fresh one; by then the
// registration and the index entry are already gone, so saveRequest refuses
// to write for the session, UpdateRegistration reports it forgotten, and
// RegisterNewSession assigns a fresh archive ID instead of reusing this one.
func (s *LocalStore) ForgetSession(archiveSessionID, nativeSessionID string) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if err := s.removeSubagentCandidatesForSession(archiveSessionID); err != nil {
		return fmt.Errorf("remove linked subagent candidates: %w", err)
	}
	paths := []string{
		s.registrationPath(archiveSessionID),
		s.requestPath(archiveSessionID),
		s.publishedPath(archiveSessionID),
		s.pendingPath(archiveSessionID),
		filepath.Join(s.home, "pending-scans", archiveSessionID+".json"),
		s.scanSignaturePath(archiveSessionID),
		s.supersededPath(archiveSessionID),
		filepath.Join(s.SessionDir(archiveSessionID), "verification.json"),
	}
	if nativeSessionID != "" {
		paths = append(paths, nativeSessionIndexPath(s.home, nativeSessionID))
	}
	// The session's candidate is gone (removed above, under this lock), so
	// nothing is left for the lock to guard.
	paths = append(paths, filepath.Join(s.home, subagentLockName(archiveSessionID)))
	// The request lock goes last. Unlinking it lets a waiting hook lock a
	// fresh file at once, so everything a hook rechecks under that lock (the
	// registration, the request, and the native-session index a new
	// registration would reuse) must already be gone by then.
	paths = append(paths, filepath.Join(s.home, requestLockName(archiveSessionID)))
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %q: %w", path, err)
		}
	}
	// Drop the per-session directory only once nothing else lives in it;
	// anything unexpected there is preserved rather than deleted blindly.
	dir := s.SessionDir(archiveSessionID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session directory %q: %w", dir, err)
	}
	if len(entries) == 0 {
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove session directory %q: %w", dir, err)
		}
	}
	return nil
}
