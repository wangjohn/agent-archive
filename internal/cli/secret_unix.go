//go:build darwin || linux

package cli

import (
	"errors"
	"io"

	"golang.org/x/sys/unix"
)

// readSecret reads a line from the terminal fd with echo off, as
// term.ReadPassword does, and restores the terminal's modes. Unlike
// term.ReadPassword it ends at the end of input: a read of no bytes is
// io.EOF, as it is for the key browser (ttyKeys.readReady).
// term.ReadPassword reads again instead, and on macOS a terminal whose
// other end has closed answers every read with no bytes, so it spun at full
// CPU forever.
func readSecret(fd int) ([]byte, error) {
	modes, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	hidden := *modes
	hidden.Lflag &^= unix.ECHO
	hidden.Lflag |= unix.ICANON | unix.ISIG
	hidden.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &hidden); err != nil {
		return nil, err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, modes) }()
	var line []byte
	var b [1]byte
	for {
		n, err := unix.Read(fd, b[:])
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return nil, err
		case n == 0:
			// A final answer with no trailing newline is still a real one.
			if len(line) > 0 {
				return line, nil
			}
			return nil, io.EOF
		}
		switch b[0] {
		case '\n':
			return line, nil
		case '\r':
		case '\b':
			if len(line) > 0 {
				line = line[:len(line)-1]
			}
		default:
			line = append(line, b[0])
		}
	}
}
