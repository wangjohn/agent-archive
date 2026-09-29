package cli

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Terminal control sequences the session browser writes: switch to and from
// the alternate screen buffer, and clear the screen with the cursor at the
// top left.
const (
	enterAltScreenSequence = "\x1b[?1049h"
	leaveAltScreenSequence = "\x1b[?1049l"
	clearScreenSequence    = "\x1b[H\x1b[2J"
)

type altScreenDependencies interface {
	isTerminal(any) bool
	interrupts() (<-chan os.Signal, func())
}

// altScreen is the browser's full-screen view: the list and a session's
// details replace each other in the terminal's alternate screen instead of
// piling up in scrollback, and leaving it restores the screen as it was. It
// does nothing when out is not a terminal. leave is safe to call more than
// once and from the interrupt handler, which restores the screen before
// the process exits on Ctrl-C or SIGTERM.
type altScreen struct {
	out    io.Writer
	mu     sync.Mutex
	active bool
	// paging is set while a pager owns the terminal. The pager handles
	// Ctrl-C itself (less uses it to cancel a search), so an interrupt then
	// is left to it.
	paging atomic.Bool
	stop   func()
	done   chan struct{}
	exit   func(int)
}

func enterAltScreen(out io.Writer, env altScreenDependencies) *altScreen {
	s := &altScreen{out: out, exit: os.Exit}
	if !env.isTerminal(out) {
		return s
	}
	signals, stop := env.interrupts()
	s.start(signals, stop)
	return s
}

// start switches to the alternate screen and restores it on the first
// interrupt outside a pager, exiting as the signal would have.
func (s *altScreen) start(signals <-chan os.Signal, stop func()) {
	s.mu.Lock()
	terminal.Print(s.out, enterAltScreenSequence)
	s.active, s.stop, s.done = true, stop, make(chan struct{})
	done := s.done
	s.mu.Unlock()
	go func() {
		for {
			select {
			case sig := <-signals:
				if sig == os.Interrupt && s.paging.Load() {
					continue
				}
				s.leave()
				code := 130
				if number, ok := sig.(syscall.Signal); ok {
					code = 128 + int(number)
				}
				s.exit(code)
				return
			case <-done:
				return
			}
		}
	}()
}

// clear blanks the screen before the next view is drawn.
func (s *altScreen) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		terminal.Print(s.out, clearScreenSequence)
	}
}

// leave returns to the normal screen, where the terminal shows what it
// showed before the browser started.
func (s *altScreen) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.active = false
	terminal.Print(s.out, leaveAltScreenSequence)
	close(s.done)
	if s.stop != nil {
		s.stop()
	}
}
