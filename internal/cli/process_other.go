//go:build !unix

package cli

// fileOwner is unknown where file ownership is not a Unix user ID.
func fileOwner(string) (uid int, ok bool) { return 0, false }
