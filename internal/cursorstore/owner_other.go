//go:build !unix

package cursorstore

import "os"

// ownedByCurrentUser is true where file ownership is not a Unix user ID; the
// per-user temporary directory is then what keeps snapshots private.
func ownedByCurrentUser(os.FileInfo) bool { return true }
