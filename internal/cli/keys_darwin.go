package cli

import "golang.org/x/sys/unix"

// The ioctl requests that read and set a terminal's modes.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// posixVDisable turns a terminal's special character off
// (_POSIX_VDISABLE).
const posixVDisable = 0xff
