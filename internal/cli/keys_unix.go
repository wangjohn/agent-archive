//go:build darwin || linux

package cli

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
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
// restores the terminal first). Ctrl-\ is turned off, so a stray key cannot
// quit; a SIGQUIT sent from outside is answered like Ctrl-C (Env.interrupts
// catches it), with the terminal restored.
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
	// SIGTSTP stays notified until release: Go keeps its own handler for
	// it once notified, and that handler ignores it, so a Ctrl-Z while a
	// pager runs would otherwise do nothing here (see whilePaging).
	signal.Notify(t.suspended, syscall.SIGTSTP)
	return nil
}

func (t *ttyKeys) lines() {
	signal.Stop(t.resized)
	t.restore()
}

func (t *ttyKeys) release() {
	signal.Stop(t.suspended)
}

func (t *ttyKeys) now() time.Time { return time.Now() }

func (t *ttyKeys) pendingStop() bool { return signalled(t.suspended) }

// whilePaging answers Ctrl-Z while a pager runs by stopping the process:
// the pager has stopped itself, and the shell sees the job stop once the
// browser has too. fg continues both; the pager still owns the screen, so
// nothing is redrawn.
func (t *ttyKeys) whilePaging() (end func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-t.suspended:
				_ = unix.Kill(os.Getpid(), unix.SIGSTOP)
				// Continued: a second Ctrl-Z already sent must not stop
				// it again at once.
				for signalled(t.suspended) {
				}
			case <-done:
				return
			}
		}
	}()
	return func() { close(done); <-finished }
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
	// A process stops itself before Kill returns; the wait for SIGCONT is
	// only a guard, so key mode never goes back on while stopped.
	select {
	case <-continued:
	case <-time.After(stopWait):
	}
	return nil
}

// stopWait bounds the guard's wait for SIGCONT, which a process continued
// after stopping itself has already had.
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
		if signalled(t.suspended) {
			return 0, errSuspended
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
func (t *ttyKeys) readReady(p []byte) (int, error) { return readTerminal(t.fd, p) }

// readTerminal reads from the terminal fd, again when a signal interrupts
// the read. A read of no bytes is io.EOF: on macOS a terminal whose other
// end has closed answers every read with no bytes, and a reader that reads
// again spins forever. The key browser and the secret prompt both read
// through it.
func readTerminal(fd int, p []byte) (int, error) {
	for {
		n, err := unix.Read(fd, p)
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

// promptLineGuard restores canonical terminal modes before hidden input exits,
// receives an interrupt, or suspends. It stays alive for the prompter lifecycle:
// Go retains its job-control handler after notification, so later ordinary
// prompts must continue handling Ctrl-Z too. It never reads stdin.
type promptLineGuard struct {
	fd          int
	mu          sync.Mutex
	original    *unix.Termios
	hiddenModes *unix.Termios
	signals     chan os.Signal
	interrupts  chan os.Signal
	done        chan struct{}
	finished    chan struct{}
	resumed     chan os.Signal
	onSuspend   func()
	onResume    func()
	delegated   atomic.Bool
}

func newPromptLineGuard(fd int, onSuspend func()) *promptLineGuard {
	g := &promptLineGuard{fd: fd, onSuspend: onSuspend, signals: make(chan os.Signal, 8), interrupts: make(chan os.Signal, 4), done: make(chan struct{}), finished: make(chan struct{}), resumed: make(chan os.Signal, 1)}
	signal.Notify(g.signals, syscall.SIGTSTP)
	signal.Notify(g.resumed, syscall.SIGCONT)
	go g.watch()
	return g
}

// screenLifecycle temporarily conceals an alternate screen during job control.
// The existing guard remains the sole owner of stopping and continuing input.
func (g *promptLineGuard) screenLifecycle(hide, show func()) func() {
	g.mu.Lock()
	oldSuspend, oldResume := g.onSuspend, g.onResume
	g.onSuspend = func() {
		if oldSuspend != nil {
			oldSuspend()
		}
		hide()
	}
	g.onResume = show
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		g.onSuspend, g.onResume = oldSuspend, oldResume
		g.mu.Unlock()
	}
}

// echoSuppressed records the user's modes before temporary prompt muting.
// ECHONL can echo only the newline even when ordinary character echo is off.
func (g *promptLineGuard) echoSuppressed() (characters, newline bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	modes, err := unix.IoctlGetTermios(g.fd, ioctlGetTermios)
	if err != nil {
		return false, false, err
	}
	return modes.Lflag&unix.ECHO == 0, modes.Lflag&(unix.ECHO|unix.ECHONL) == 0, nil
}

func (g *promptLineGuard) hidden() (func(), error) {
	return g.hideEcho(true)
}

// muted suppresses echo only while a block is emitted. Canonical editing,
// EOF and signal processing retain their existing terminal settings.
func (g *promptLineGuard) muted() (func(), error) {
	return g.hideEcho(false)
}

func (g *promptLineGuard) hideEcho(canonical bool) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	modes, err := unix.IoctlGetTermios(g.fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	hidden := *modes
	hidden.Lflag &^= unix.ECHO | unix.ECHONL
	if canonical {
		hidden.Lflag |= unix.ICANON | unix.ISIG
		hidden.Iflag |= unix.ICRNL
	}
	if err := unix.IoctlSetTermios(g.fd, ioctlSetTermios, &hidden); err != nil {
		return nil, err
	}
	g.original = modes
	g.hiddenModes = &hidden
	signal.Notify(g.interrupts, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	return func() {
		signal.Stop(g.interrupts)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.restore()
		g.original = nil
		g.hiddenModes = nil
	}, nil
}

func (g *promptLineGuard) restore() {
	if g.original != nil {
		_ = unix.IoctlSetTermios(g.fd, ioctlSetTermios, g.original)
	}
}

func (g *promptLineGuard) watch() {
	for {
		select {
		case <-g.signals:
			if g.delegated.Load() {
				continue
			}
			g.mu.Lock()
			g.restore()
			if g.onSuspend != nil {
				g.onSuspend()
			}
			select {
			case <-g.resumed:
			default:
			}
			// SIGSTOP delivery may race this goroutine on another runtime thread.
			// Keep echo restored until SIGCONT confirms the process resumed.
			if err := unix.Kill(os.Getpid(), unix.SIGSTOP); err == nil {
				<-g.resumed
			}
			if g.onResume != nil {
				g.onResume()
			}
			if g.hiddenModes != nil {
				_ = unix.IoctlSetTermios(g.fd, ioctlSetTermios, g.hiddenModes)
			}
			g.mu.Unlock()
		case sig := <-g.interrupts:
			g.mu.Lock()
			g.restore()
			g.mu.Unlock()
			os.Exit(128 + int(sig.(syscall.Signal)))
		case <-g.done:
			g.mu.Lock()
			g.restore()
			g.mu.Unlock()
			close(g.finished)
			return
		}
	}
}

func (g *promptLineGuard) close() {
	signal.Stop(g.signals)
	signal.Stop(g.interrupts)
	close(g.done)
	<-g.finished
	signal.Stop(g.resumed)
}

// terminalInputPending detects canonical lines already echoed for a later
// prompt, without consuming any input or creating another reader.
func terminalInputPending(fd int) bool {
	var fds unix.FdSet
	fds.Set(fd)
	timeout := unix.Timeval{}
	n, err := unix.Select(fd+1, &fds, nil, nil, &timeout)
	return err == nil && n > 0
}
