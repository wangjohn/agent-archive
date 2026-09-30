//go:build !unix

package cli

import "os/exec"

// fileOwner is unknown where file ownership is not a Unix user ID.
func fileOwner(string) (uid int, ok bool) { return 0, false }

// ownProcessGroup does nothing where there are no Unix process groups.
func ownProcessGroup(*exec.Cmd) {}
