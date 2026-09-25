// Package local provides private durable files and process coordination.
package local

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ID returns a new random identifier: 16 bytes from crypto/rand, hex encoded
// as 32 lowercase characters. It is used for machine IDs and tokens.
func ID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}

// Home returns the archive's local state directory, creating it if needed
// and setting its mode to 0700. It is $AGENT_ARCHIVE_HOME when set, otherwise
// ~/.local/share/agent-archive, with symlinks resolved. It refuses a directory
// inside a Git checkout, so archive state never lands in a repository.
func Home() (string, error) { return resolveHome(true) }

// ReadHome is Home without creating or changing the directory, for callers
// that only read state and must not leave one behind.
func ReadHome() (string, error) { return resolveHome(false) }

func resolveHome(create bool) (string, error) {
	path := os.Getenv("AGENT_ARCHIVE_HOME")
	if path == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		path = filepath.Join(home, ".local", "share", "agent-archive")
	}
	path, e := ResolveExistingSymlinks(path)
	if e != nil {
		return "", errors.New("invalid archive directory")
	}
	for p := path; ; p = filepath.Dir(p) {
		if _, e = os.Stat(filepath.Join(p, ".git")); e == nil {
			return "", fmt.Errorf("the archive's data directory %s would be inside the Git checkout %s, and archive storage must stay outside Git checkouts; set AGENT_ARCHIVE_HOME to a directory outside %s", path, p, p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if !create {
		return path, nil
	}
	if e = os.MkdirAll(path, 0700); e != nil {
		return "", e
	}
	if e = os.Chmod(path, 0700); e != nil {
		return "", e
	}
	return path, nil
}

// ResolveExistingSymlinks returns path made absolute with symlinks resolved
// through its deepest existing ancestor, so a path that does not exist yet
// still lands where it will really be created. Paths compared lexically
// elsewhere (project roots, the archive directory) must go through this
// first, or a symlinked spelling never matches the real one.
func ResolveExistingSymlinks(path string) (string, error) {
	path, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	ancestor := path
	for {
		if _, e = os.Stat(ancestor); e == nil {
			break
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", errors.New("no existing ancestor for " + path)
		}
		ancestor = next
	}
	resolved, e := filepath.EvalSymlinks(ancestor)
	if e != nil {
		return "", e
	}
	rel, e := filepath.Rel(ancestor, path)
	if e != nil {
		return "", e
	}
	return filepath.Join(resolved, rel), nil
}

// Write stores value as indented JSON with a trailing newline, atomically,
// as WriteBytes does.
func Write(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	return writeAtomic(path, func(w io.Writer) error {
		if _, e := w.Write(b); e != nil {
			return e
		}
		_, e := w.Write([]byte{'\n'})
		return e
	})
}

// WriteCompact stores value as compact JSON (json.Marshal's bytes) with a
// trailing newline, atomically, as WriteBytes does. It is the one to use for
// a value that can be megabytes of JSON (a source bundle): Write indents the
// encoded document into a second copy, and the file comes out a third
// larger, where this encodes it once, in the encoder's buffer, and writes
// that.
func WriteCompact(path string, value any) error {
	return writeAtomic(path, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(value)
	})
}

// WriteBytes replaces path with b atomically: it writes a 0600 temporary file
// in the same directory (created 0700 if missing), syncs it, renames it over
// path, and syncs the directory. A reader sees the old content or the new,
// never a partial file.
func WriteBytes(path string, b []byte) error {
	return writeAtomic(path, func(w io.Writer) error {
		_, e := w.Write(b)
		return e
	})
}

// writeAtomic is WriteBytes with the content written by write, which must
// report any failure to write it: nothing is renamed over path unless it
// returns nil.
func writeAtomic(path string, write func(io.Writer) error) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), tempPrefix)
	if e != nil {
		return e
	}
	// After a successful rename there is nothing left to remove.
	defer func() { _ = os.Remove(f.Name()) }()
	if e = f.Chmod(0600); e == nil {
		e = write(f)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// tempPrefix names WriteBytes' temporary files. A process that dies between
// creating one and renaming it over its target leaves it behind.
const tempPrefix = ".pending-"

// RemoveStaleTemps removes the temporary files WriteBytes left in dir, not
// in its subdirectories, whose modification time is more than olderThan ago.
// A write in progress is younger than any sensible olderThan, so only a
// temporary a crashed writer abandoned is removed. A missing dir is not an
// error.
func RemoveStaleTemps(dir string, olderThan time.Duration) error {
	entries, e := os.ReadDir(dir)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	cutoff := time.Now().Add(-olderThan)
	var errs []error
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), tempPrefix) {
			continue
		}
		info, e := entry.Info()
		if e != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if e := os.Remove(filepath.Join(dir, entry.Name())); e != nil && !errors.Is(e, os.ErrNotExist) {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

// TrimLog keeps an append-only log file from growing without bound: once it
// is larger than maxBytes it is cut down, in place, to about its last keep
// bytes, starting at a line boundary. The file is truncated rather than
// replaced so a writer holding it open with O_APPEND (launchd's redirect of
// the collector's stderr) keeps writing to the same file. It must not run
// while another process writes the file. A missing file is not an error.
func TrimLog(path string, maxBytes, keep int64) (e error) {
	f, e := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer func() {
		if ce := f.Close(); ce != nil && e == nil {
			e = ce
		}
	}()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() || info.Size() <= maxBytes {
		return nil
	}
	keep = min(keep, info.Size())
	tail := make([]byte, keep)
	if _, e = f.ReadAt(tail, info.Size()-keep); e != nil {
		return e
	}
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	if e = f.Truncate(0); e != nil {
		return e
	}
	if _, e = f.WriteAt(tail, 0); e != nil {
		return e
	}
	return f.Sync()
}

// Read decodes the JSON file at path into value. A missing file returns an
// error that satisfies errors.Is(err, os.ErrNotExist).
func Read(path string, value any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, value)
}

// ErrBusy means a lock is held by another process, such as a collector or
// setup run already in progress.
var ErrBusy = errors.New("another collector or setup is running")

// Lock takes the collector lock, home/collector.lock, which serializes the
// collector with setup and other writers of archive state. It does not wait:
// ErrBusy means another process holds it. See NamedLock.
func Lock(home string) (func(), error) { return NamedLock(home, "collector.lock") }

// NamedLock takes an exclusive, non-blocking flock on home/name, creating the
// file if needed, and returns the function that releases it; ErrBusy means
// another holder has it.
//
// A lock file may be unlinked while it is held (ForgetSession removes a
// session's lock files). Someone who opened the file before the unlink would
// then lock the orphaned inode while a newcomer locks a fresh file at the
// same path, and both would believe they hold the lock. So once the flock is
// taken, the path must still name the very file that was locked; if it does
// not, the lock is dropped and taken again on whatever the path names now.
func NamedLock(home, name string) (func(), error) {
	path := filepath.Join(home, name)
	for range lockAttempts {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		release, current, e := lockOpened(path, f)
		if e != nil || current {
			return release, e
		}
		// Unlinked (or replaced) between the open and the lock: try again.
	}
	return nil, fmt.Errorf("lock %s: the file kept being replaced while it was locked", path)
}

// lockAttempts bounds NamedLock's retries on a lock file that is unlinked or
// replaced between its open and its lock. Each retry means another process
// removed the file just then; far fewer than this happen in practice.
const lockAttempts = 100

// lockOpened flocks f, which was opened at path, and reports whether path
// still names f's file once the lock is held. When it does not, or on error,
// f is unlocked and closed; otherwise release does both.
func lockOpened(path string, f *os.File) (release func(), current bool, e error) {
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = f.Close()
		if errors.Is(e, syscall.EWOULDBLOCK) || errors.Is(e, syscall.EAGAIN) {
			return nil, false, ErrBusy
		}
		return nil, false, e
	}
	release = func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	locked, e := f.Stat()
	if e != nil {
		release()
		return nil, false, e
	}
	named, e := os.Stat(path)
	if e == nil && os.SameFile(locked, named) {
		return release, true, nil
	}
	release()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, false, e
	}
	return nil, false, nil
}

// NamedLockWait is NamedLock retried every 10ms until timeout, so a caller
// with a deadline (a hook) tolerates short contention without overrunning it.
// When the lock is still held at the deadline it returns ErrBusy.
func NamedLockWait(home, name string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		unlock, err := NamedLock(home, name)
		if !errors.Is(err, ErrBusy) {
			return unlock, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, err
		}
		time.Sleep(min(10*time.Millisecond, remaining))
	}
}
