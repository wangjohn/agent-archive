//go:build darwin || linux

package cli

import (
	"errors"
	"io"

	"golang.org/x/sys/unix"
)

// readSecret reads a line from the terminal fd with echo off, as
// term.ReadPassword does, and restores the terminal's modes. Unlike
// term.ReadPassword it ends at the end of input (see readTerminal):
// term.ReadPassword reads again after a read of no bytes, so it spun at
// full CPU forever on a macOS terminal whose other end had closed.
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
		_, err := readTerminal(fd, b[:])
		switch {
		case errors.Is(err, io.EOF):
			// A final answer with no trailing newline is still a real one.
			if len(line) > 0 {
				return line, nil
			}
			return nil, io.EOF
		case err != nil:
			return nil, err
		}
		// ICRNL above turns Enter's CR into the NL that ends the line.
		switch b[0] {
		case '\n':
			return line, nil
		case '\b':
			if len(line) > 0 {
				line = line[:len(line)-1]
			}
		default:
			line = append(line, b[0])
		}
	}
}
