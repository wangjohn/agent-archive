//go:build !unix

package credentials

import "os"

// The file store is not usable off Unix: there is no permission mode to test
// (Go reports a synthetic one that always has group and world bits), so Load
// and Save refuse with ErrInsecurePermissions and fail closed rather than
// keep a secret nobody can vouch for. Nothing here builds a working store.
//
// ownedByUser is true where file ownership is not a Unix user ID.
func ownedByUser(os.FileInfo, int) bool { return true }

// openNoFollow opens path for reading. Symbolic links are refused before the
// open, by the caller's lstat, where O_NOFOLLOW does not exist.
func openNoFollow(path string) (*os.File, error) { return os.Open(path) } //nolint:gosec // G304: inside the credentials folder, a validated name.

// isSymlinkLoop is never true here: there is no O_NOFOLLOW to fail.
func isSymlinkLoop(error) bool { return false }
