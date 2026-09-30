//go:build !unix

package host

import "os/exec"

// ownProcessGroup does nothing where there are no Unix process groups.
func ownProcessGroup(*exec.Cmd) {}
