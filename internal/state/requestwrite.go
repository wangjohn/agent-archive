package state

import (
	"bytes"
	"errors"
	"os"

	"github.com/wangjohn/agent-archive/internal/local"
)

// fileSnapshot is a file's content as one read saw it; found is false when
// there was no file.
type fileSnapshot struct {
	data  []byte
	found bool
}

func readSnapshot(path string) (fileSnapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{data: data, found: true}, nil
}

func (f fileSnapshot) equal(other fileSnapshot) bool {
	return f.found == other.found && bytes.Equal(f.data, other.data)
}

// requestWriteAttempts bounds how many times writeUnderRequestLock tries a
// write. Each attempt costs one disk sync, and a conflict needs another
// writer to commit to the same session's file within it (hooks are
// serialized by hooks.lock, so one side is the collector, backfill, or
// feedback). A writer overtaken on every earlier attempt makes its last one
// holding the lock, so a writer that keeps committing cannot starve it.
const requestWriteAttempts = 4

// writeUnderRequestLock replaces path, one of the files a session's request
// lock guards (its request or its registration), with what change computes
// from the file's current content, holding that lock only to check and
// rename, never across a disk sync.
//
// Hooks wait a second for the request lock (lockRequest), inside the
// harness's two-second hook budget, and the turn's evidence is lost when the
// lock stays held longer. A durable write syncs the file and then its
// directory, each an F_FULLFSYNC on macOS, and together they have been
// measured past two seconds on a loaded Mac. So no holder of the lock may
// sync under it:
//
//  1. The file is read without the lock, change computes the new content
//     from it, and that content is written and synced to a temporary file
//     beside path (local.Stage).
//  2. Under the lock, check runs, and the file is read again. If it still
//     holds exactly what change saw, the temporary is renamed over it: the
//     commit point, which every other holder of the lock sees atomically,
//     just as it saw the whole read-modify-write before. If another writer
//     changed it meanwhile, the temporary is discarded and the next attempt
//     computes from the new content.
//  3. After the lock is released, and before this returns, the directory is
//     synced, making the rename durable.
//
// The comparison is on content, not on a token or file identity: change is
// a function of the content alone, so a file changed and changed back yields
// the same write. That keeps every guarantee the lock gave. A hook landing
// while the collector publishes still gets a fresh token (saveRequest), so
// CompleteRequest leaves its evidence pending; and a forget under the lock
// cannot slip between a writer's registration check and its write, because
// check runs under the lock right before the rename.
//
// Between the rename and the directory sync, another holder of the lock can
// read the new file before it is durable. The writer has not returned yet,
// so it has promised nothing: a crash then can undo the rename, as it could
// undo a write that never finished, and what that reader did with the
// content (the collector publishing a hook's evidence, retention keeping a
// session) was still correct for content that existed. A reader never sees
// a partial file, as the temporary is synced before it is renamed.
//
// The last of requestWriteAttempts holds the lock from its read through its
// rename, and so across the temporary file's sync: without it, a writer
// committing back to back beats every staged attempt of another (a stress
// run with the collector writing continuously lost hook writes that way).
// That hold happens only after a writer was overtaken on every earlier
// attempt, and the directory sync still waits until the lock is released.
//
// change may run more than once, so it must compute only from its argument
// and the caller's inputs, and record its results afresh on every run.
// write false leaves the file as it is; check still runs under the lock and
// the file must still be unchanged, so a decision not to write is as atomic
// as a write. An error from change or check is returned as is, with nothing
// written.
func (s *Store) writeUnderRequestLock(archiveSessionID, path string, check func() error, change func(current fileSnapshot) (value any, write bool, err error)) error {
	for range requestWriteAttempts - 1 {
		before, err := readSnapshot(path)
		if err != nil {
			return err
		}
		value, write, err := change(before)
		if err != nil {
			return err
		}
		var staged *local.Staged
		if write {
			if staged, err = local.Stage(path, value); err != nil {
				return err
			}
			s.requestWriteSynced()
		}
		committed, err := s.commitUnderRequestLock(archiveSessionID, path, before, staged, check)
		staged.Discard()
		if err != nil {
			return err
		}
		if !committed {
			continue
		}
		if staged == nil {
			return nil
		}
		s.requestWriteSynced()
		return staged.SyncDir()
	}
	return s.writeHoldingRequestLock(archiveSessionID, path, check, change)
}

// writeHoldingRequestLock is writeUnderRequestLock's last attempt: the read,
// change, and staging happen under the lock, so no other writer can overtake
// it. Only the directory sync waits until the lock is released.
func (s *Store) writeHoldingRequestLock(archiveSessionID, path string, check func() error, change func(current fileSnapshot) (value any, write bool, err error)) error {
	staged, err := func() (*local.Staged, error) {
		unlock, err := s.lockRequest(archiveSessionID)
		if err != nil {
			return nil, err
		}
		defer unlock()
		if check != nil {
			if err := check(); err != nil {
				return nil, err
			}
		}
		current, err := readSnapshot(path)
		if err != nil {
			return nil, err
		}
		value, write, err := change(current)
		if err != nil || !write {
			return nil, err
		}
		staged, err := local.Stage(path, value)
		if err != nil {
			return nil, err
		}
		if err := staged.Commit(); err != nil {
			staged.Discard()
			return nil, err
		}
		return staged, nil
	}()
	if err != nil || staged == nil {
		return err
	}
	s.requestWriteSynced()
	return staged.SyncDir()
}

// commitUnderRequestLock is writeUnderRequestLock's step under the lock.
// committed is false when the file no longer holds before.
func (s *Store) commitUnderRequestLock(archiveSessionID, path string, before fileSnapshot, staged *local.Staged, check func() error) (committed bool, err error) {
	unlock, err := s.lockRequest(archiveSessionID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if check != nil {
		if err := check(); err != nil {
			return false, err
		}
	}
	current, err := readSnapshot(path)
	if err != nil || !current.equal(before) {
		return false, err
	}
	if staged == nil {
		return true, nil
	}
	return true, staged.Commit()
}

// requestWriteSynced runs the test seam, if any, where writeUnderRequestLock
// syncs outside the lock.
func (s *Store) requestWriteSynced() {
	if s.onRequestWriteSync != nil {
		s.onRequestWriteSync()
	}
}
