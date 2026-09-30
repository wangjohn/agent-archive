package cli

import "golang.org/x/sys/unix"

// The ioctl requests that read and set a terminal's modes.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// ioctlSetTermiosFlush sets a terminal's modes after discarding the input
// not read yet.
const ioctlSetTermiosFlush = unix.TCSETSF

// posixVDisable turns a terminal's special character off
// (_POSIX_VDISABLE).
const posixVDisable = 0
