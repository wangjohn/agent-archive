package credentials

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CredentialsDirName is the folder under the data directory that holds one
// file per credential reference when the credentials are kept in files (a
// non-macOS build; see OpenDefault).
const CredentialsDirName = "credentials"

// FileStoreDir is where the credential files live for a data directory.
func FileStoreDir(dataDir string) string { return filepath.Join(dataDir, CredentialsDirName) }

// storeSentinelError is a credential-file failure that is also an instance of
// a broader error, so callers that only know the broader one keep working: a
// missing credentials file is still ErrMissingCredential, and an insecure or
// unreadable one is still ErrUnavailable. It mirrors the Keychain's sentinels
// (keychainSentinelError).
type storeSentinelError struct {
	message string
	parent  error
}

func (e *storeSentinelError) Error() string { return e.message }

func (e *storeSentinelError) Unwrap() error { return e.parent }

var (
	// ErrCredentialFileNotFound means the credentials folder holds no file
	// for the reference. Re-running setup stores it again. It is the only
	// failure the environment fallback (OpenDefault) answers: a file that
	// exists but cannot be trusted is never bypassed.
	ErrCredentialFileNotFound error = &storeSentinelError{message: "storage credential not found in the credentials file", parent: ErrMissingCredential}
	// ErrInsecurePermissions means a credentials file, or the folder that
	// holds it, is open to other users, is a symbolic link, is owned by
	// someone else, or is not a regular file or folder. The file is not read
	// and nothing is written to the folder. The wrapping error names the path
	// and the command that fixes it.
	ErrInsecurePermissions error = &storeSentinelError{message: "credentials file is not private", parent: ErrUnavailable}
	// ErrCredentialFileUnreadable means a credentials file could not be read
	// or does not hold an access key ID and secret. Its content is never
	// included in the error.
	ErrCredentialFileUnreadable error = &storeSentinelError{message: "credentials file could not be read", parent: ErrUnavailable}
)

// maxCredentialFileBytes bounds what Load reads: a credential is a few
// hundred bytes of JSON.
const maxCredentialFileBytes = 64 << 10

// credentialReference is every reference a file store accepts. It is what
// keeps a reference from naming anything but one file in the folder: no
// separators, no ".." (a reference starts with a letter or digit), no NUL,
// no spaces. setup's references ("setup-<32 hex digits>") and its probe
// ("agent-archive-setup-check") are inside it.
var credentialReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateFileReference(reference string) error {
	if reference == "" {
		return ErrInvalidReference
	}
	if !credentialReference.MatchString(reference) {
		// The reference is not echoed: it can come from a configuration
		// file, and this message goes to a terminal.
		return fmt.Errorf("%w: a reference is letters, digits, '.', '_' and '-' (at most 128, starting with a letter or digit)", ErrInvalidReference)
	}
	return nil
}

// FileStore keeps each credential in its own file, <dir>/<reference>.json,
// readable only by the user running agent-archive: the store used where there
// is no Keychain. The file holds the same JSON EncodeSecret produces for the
// Keychain.
//
// The secret is on disk, so the store refuses to trust anything another user
// could have read or replaced. Load returns ErrInsecurePermissions unless the
// folder and the file are both owned by this user, are not group- or
// world-accessible (mode & 0o077 == 0), and the file is a regular file, not a
// symbolic link. Save applies the same test to the folder and never creates a
// file wider than 0600, not even for an instant. The folder is created, with
// mode 0700, by the first Save, not by NewFileStore or Load, so a read-only
// command leaves nothing behind.
type FileStore struct {
	dir string
	// uid is the user that must own the folder and the files: this process's
	// effective user. A test replaces it, since a file owned by another user
	// cannot be made without root.
	uid func() int
	// now is the clock the stale temporary file sweep reads.
	now func() time.Time
	// tempCreated, when set, is called with the temporary file's path right
	// after it is created, before anything is written to it. A test uses it
	// to see the mode the file is created with.
	tempCreated func(path string)
}

// NewFileStore returns a store for the credential files in dir, which must
// be an absolute path, ordinarily FileStoreDir of the data directory. It does
// not touch the file system.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, errors.New("credentials folder must be an absolute path")
	}
	return &FileStore{dir: filepath.Clean(dir), uid: os.Geteuid, now: time.Now}, nil
}

func (s *FileStore) path(reference string) string {
	return filepath.Join(s.dir, reference+".json")
}

// shellQuote quotes s for a POSIX shell, so a path with a space or an
// apostrophe in a fix command is still one word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// insecureError is a refusal for permissions. Whichever check refuses (the
// file or its folder, on Load, Save or Delete), it is the same set of errors:
// ErrInsecurePermissions, ErrUnavailable, and ErrCredentialFileUnreadable.
func insecureError(path, problem, fix string) error {
	return insecurePermissionsError{fmt.Errorf("%w: %s %s; %s", ErrInsecurePermissions, path, problem, fix)}
}

// checkInfo is the test every folder and file passes before it is trusted:
// not a symbolic link, the right kind, owned by this user, and (when
// checkMode) closed to group and world. wantDir says which kind. It works on
// an os.Lstat result for the path, or an os.File's Stat for an opened file.
func (s *FileStore) checkInfo(path string, info os.FileInfo, wantDir, checkMode bool) error {
	mode := info.Mode()
	kind, fixMode := "file", "chmod 600 "+shellQuote(path)
	if wantDir {
		kind, fixMode = "folder", "chmod 700 "+shellQuote(path)
	}
	remove := "delete it, then run agent-archive setup and choose storage to save the credential again"
	switch {
	case mode&os.ModeSymlink != 0:
		return insecureError(path, "is a symbolic link", remove)
	case wantDir && !mode.IsDir(), !wantDir && !mode.IsRegular():
		return insecureError(path, "is not a regular "+kind, remove)
	case !ownedByUser(info, s.uid()):
		return insecureError(path, "is owned by another user", "it must belong to the user running agent-archive; "+remove)
	case checkMode && mode.Perm()&0o077 != 0:
		return insecureError(path, fmt.Sprintf("is accessible by other users (mode %04o)", uint32(mode.Perm())), "run: "+fixMode)
	}
	return nil
}

// checkDir lstats the folder and applies checkInfo. A missing folder is
// returned as the os error, for the caller to decide about.
func (s *FileStore) checkDir(checkMode bool) error {
	info, err := os.Lstat(s.dir)
	if err != nil {
		return err
	}
	return s.checkInfo(s.dir, info, true, checkMode)
}

// Save stores value under reference, replacing any existing file, however it
// was written. The file is written to a temporary file in the same folder,
// created with mode 0600 and never wider, flushed, and renamed over the
// target, so a reader sees the old file or the whole new one.
func (s *FileStore) Save(ctx context.Context, reference string, value R2Credentials) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = validateFileReference(reference); err != nil {
		return err
	}
	encoded, err := EncodeSecret(value)
	if err != nil {
		return err
	}
	if err = s.ensureDir(); err != nil {
		return err
	}
	s.sweepStaleTemps()
	suffix := make([]byte, 16)
	if _, err = rand.Read(suffix); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	tmpPath := filepath.Join(s.dir, tempPrefix+hex.EncodeToString(suffix))
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: inside the credentials folder, a validated name.
	if err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	// From here the temporary file is removed on every failure.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if s.tempCreated != nil {
		s.tempCreated(tmpPath)
	}
	// The open already asked for 0600, which a umask can only narrow; this
	// makes the mode exactly 0600 whatever the umask, before any secret is
	// written.
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(encoded)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	if err = os.Rename(tmpPath, s.path(reference)); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	// Flush the rename. A file system that cannot sync a folder has already
	// stored the file, so a failure here is not one.
	if dir, openErr := os.Open(s.dir); openErr == nil { //nolint:gosec // G304: the credentials folder.
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// staleTempAge is how old a temporary file must be before a Save removes it.
// A Save holds its temporary file for milliseconds, so an hour-old one was
// left by a process that died between creating and renaming it.
const staleTempAge = time.Hour

// tempPrefix starts every temporary file's name; no reference can (a
// reference starts with a letter or digit).
const tempPrefix = ".tmp-"

// sweepStaleTemps removes the temporary files a killed Save left behind: a
// regular file (a link is not followed, and is left alone), named like a
// temporary file, owned by this user, and older than staleTempAge. It never
// fails a Save.
func (s *FileStore) sweepStaleTemps() {
	list, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := s.now().Add(-staleTempAge)
	for _, entry := range list {
		if !strings.HasPrefix(entry.Name(), tempPrefix) {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !ownedByUser(info, s.uid()) || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(path)
	}
}

// ensureDir makes the folder if it is absent (mode 0700), and requires that
// what is there is private.
func (s *FileStore) ensureDir() error {
	err := s.checkDir(true)
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create credentials folder: %w", err)
	}
	return s.checkDir(true)
}

// Load returns the credentials stored under reference. It refuses, with an
// ErrInsecurePermissions that names the fix, a file or folder another user
// could read or have replaced (see FileStore).
func (s *FileStore) Load(ctx context.Context, reference string) (R2Credentials, error) {
	if err := ctx.Err(); err != nil {
		return R2Credentials{}, err
	}
	if err := validateFileReference(reference); err != nil {
		return R2Credentials{}, err
	}
	if err := s.checkDir(true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return R2Credentials{}, ErrCredentialFileNotFound
		}
		return R2Credentials{}, s.unreadable(err)
	}
	path := s.path(reference)
	// The path is inspected before it is opened, for an exact message, and
	// the opened file is inspected again: the descriptor, not the name, is
	// what is trusted.
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return R2Credentials{}, ErrCredentialFileNotFound
	}
	if err != nil {
		return R2Credentials{}, s.unreadable(err)
	}
	if err = s.checkInfo(path, info, false, true); err != nil {
		return R2Credentials{}, err
	}
	file, err := openNoFollow(path)
	if err != nil {
		if isSymlinkLoop(err) {
			return R2Credentials{}, insecureError(path, "is a symbolic link", "delete it, then run agent-archive setup and choose storage to save the credential again")
		}
		if errors.Is(err, os.ErrNotExist) {
			return R2Credentials{}, ErrCredentialFileNotFound
		}
		return R2Credentials{}, s.unreadable(err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return R2Credentials{}, s.unreadable(err)
	}
	if err = s.checkInfo(path, opened, false, true); err != nil {
		return R2Credentials{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil {
		return R2Credentials{}, s.unreadable(err)
	}
	if len(data) > maxCredentialFileBytes {
		return R2Credentials{}, fmt.Errorf("%w: %s is larger than a credential can be", ErrCredentialFileUnreadable, path)
	}
	value, err := DecodeSecret(data)
	if err != nil {
		return R2Credentials{}, fmt.Errorf("%w: %s does not hold an access key ID and secret; run agent-archive setup and choose storage to save the credential again", ErrCredentialFileUnreadable, path)
	}
	return value, nil
}

// unreadable is an ErrCredentialFileUnreadable that carries the operating
// system's error, which names a path and a reason and never a file's content.
// A refusal for permissions (see insecureError) is already a full message, so
// it is not prefixed again.
func (s *FileStore) unreadable(err error) error {
	if errors.Is(err, ErrInsecurePermissions) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrCredentialFileUnreadable, err)
}

// insecurePermissionsError is an error that keeps its own message and is also an
// ErrCredentialFileUnreadable.
type insecurePermissionsError struct{ error }

func (e insecurePermissionsError) Is(target error) bool { return target == ErrCredentialFileUnreadable }

func (e insecurePermissionsError) Unwrap() error { return e.error }

// Delete removes the file under reference; an absent file is not an error.
// It does not follow a symbolic link, and refuses a folder that is not this
// user's own, since removing through a substituted folder would delete
// someone else's file. A folder that is merely open to other users does not
// stop it: removing a secret is never the danger.
func (s *FileStore) Delete(ctx context.Context, reference string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateFileReference(reference); err != nil {
		return err
	}
	if err := s.checkDir(false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return s.unreadable(err)
	}
	path := s.path(reference)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return s.unreadable(err)
	}
	if info.IsDir() {
		return insecureError(path, "is a folder, not a credentials file", "move it away, then run agent-archive setup again")
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return s.unreadable(err)
	}
	return nil
}

var _ CredentialStore = (*FileStore)(nil)
