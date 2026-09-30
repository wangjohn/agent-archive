//go:build darwin || linux

package cli

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// ttyKeys is a terminal on stdin read a key at a time.
type ttyKeys struct {
	fd int
	// restore puts back the modes the terminal had when it was opened.
	restore func()
	// resized delivers SIGWINCH while key mode is on.
	resized chan os.Signal
	// suspended delivers SIGTSTP (Ctrl-Z) while key mode is on.
	suspended chan os.Signal
}

// terminalKeys is Env.openKeyTerminal's default (openTerminalKeys): stdin,
// when it is a terminal whose modes can be read.
func terminalKeys(stdin io.Reader) (keyTerminal, bool) {
	file, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil, false
	}
	fd := int(file.Fd())
	if _, err := unix.IoctlGetTermios(fd, ioctlGetTermios); err != nil {
		return nil, false
	}
	return &ttyKeys{
		fd:        fd,
		restore:   saveTerminalState(stdin),
		resized:   make(chan os.Signal, 1),
		suspended: make(chan os.Signal, 1),
	}, true
}

// keys turns off canonical input and echo, reading each byte as it comes
// (VMIN 1, VTIME 0), and the extended input characters (IEXTEN: macOS's
// Ctrl-O would discard output, Ctrl-V quote the next key). ISIG stays on,
// so Ctrl-C still sends SIGINT, which the browser's interrupt handler
// answers by restoring the terminal, and Ctrl-Z suspends (keyInput
// restores the terminal first). Ctrl-\ is turned off: its SIGQUIT would
// end the process with no chance to restore the terminal.
func (t *ttyKeys) keys() error {
	modes, err := unix.IoctlGetTermios(t.fd, ioctlGetTermios)
	if err != nil {
		return err
	}
	modes.Lflag &^= unix.ICANON | unix.ECHO | unix.ECHONL | unix.IEXTEN
	modes.Lflag |= unix.ISIG
	modes.Cc[unix.VMIN] = 1
	modes.Cc[unix.VTIME] = 0
	modes.Cc[unix.VQUIT] = posixVDisable
	if err := unix.IoctlSetTermios(t.fd, ioctlSetTermios, modes); err != nil {
		return err
	}
	signal.Notify(t.resized, syscall.SIGWINCH)
	signal.Notify(t.suspended, syscall.SIGTSTP)
	return nil
}

func (t *ttyKeys) lines() {
	signal.Stop(t.resized)
	signal.Stop(t.suspended)
	t.restore()
}

// flush discards input not read yet, keeping the terminal's modes.
func (t *ttyKeys) flush() {
	if modes, err := unix.IoctlGetTermios(t.fd, ioctlGetTermios); err == nil {
		_ = unix.IoctlSetTermios(t.fd, ioctlSetTermiosFlush, modes)
	}
}

// stop stops the process as Ctrl-Z would have, and returns once the shell
// continues it. It sends SIGSTOP: Go keeps its own handler for SIGTSTP
// after signal.Reset, so re-raising SIGTSTP would not stop the process.
func (t *ttyKeys) stop() error {
	continued := make(chan os.Signal, 1)
	signal.Notify(continued, syscall.SIGCONT)
	defer signal.Stop(continued)
	if err := unix.Kill(os.Getpid(), unix.SIGSTOP); err != nil {
		return err
	}
	// The stop takes effect as the signal is delivered, which may be just
	// after Kill returns; key mode goes back on only once continued.
	select {
	case <-continued:
	case <-time.After(stopWait):
	}
	return nil
}

// stopWait bounds how long stop waits for SIGCONT, in case it never
// comes; a stopped process gets it as it continues.
const stopWait = time.Second

// readPoll is how often a read waiting forever checks for a resize or
// Ctrl-Z.
const readPoll = 100 * time.Millisecond

func (t *ttyKeys) read(p []byte, wait time.Duration) (int, error) {
	deadline := time.Now().Add(wait)
	for {
		slice := readPoll
		if wait >= 0 {
			slice = min(slice, max(time.Until(deadline), 0))
		}
		var fds unix.FdSet
		fds.Set(t.fd)
		timeout := unix.NsecToTimeval(slice.Nanoseconds())
		n, err := unix.Select(t.fd+1, &fds, nil, nil, &timeout)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return 0, err
		}
		if n > 0 {
			return t.readReady(p)
		}
		if wait < 0 {
			if signalled(t.suspended) {
				return 0, errSuspended
			}
			if signalled(t.resized) {
				return 0, errWindowResized
			}
			continue
		}
		if !time.Now().Before(deadline) {
			return 0, nil
		}
	}
}

// signalled reports whether a signal came on signals since it was last
// asked.
func signalled(signals chan os.Signal) bool {
	select {
	case <-signals:
		return true
	default:
		return false
	}
}

// readReady reads the input select said is waiting.
func (t *ttyKeys) readReady(p []byte) (int, error) {
	for {
		n, err := unix.Read(t.fd, p)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return 0, err
		case n == 0:
			return 0, io.EOF
		}
		return n, nil
	}
}
