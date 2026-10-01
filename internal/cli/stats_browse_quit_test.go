package cli

import (
	"io"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// restoredSuffix is what the screen's output ends with once it has been left:
// the cursor shown, then the normal screen.
const restoredSuffix = showCursorSequence + leaveAltScreenSequence

// Every way of quitting (q, Esc, Ctrl-D, the end of input) gives the terminal
// its modes, its cursor and its screen back, releases the interrupt handler,
// and exits 0.
func TestStatsScreenQuitRestoresTheTerminal(t *testing.T) {
	t.Parallel()
	for _, chunks := range [][]string{{"q"}, {"Q"}, {"\x1b"}, {"\x04"}, {}, {"d", "?", "q", "q"}} {
		run := runScreen(t, screenOptions{}, chunks...)
		if !run.ran || run.code != 0 || run.stderr != "" {
			t.Errorf("%q: ran=%v code=%d stderr=%q", chunks, run.ran, run.code, run.stderr)
		}
		if history := run.fake.history(); !reflect.DeepEqual(history, []string{"keys", "flush", "lines", "release"}) {
			t.Errorf("%q: terminal modes %v", chunks, history)
		}
		out := run.out.String()
		if !strings.HasPrefix(out, enterAltScreenSequence) || !strings.HasSuffix(out, restoredSuffix) || strings.Count(out, leaveAltScreenSequence) != 1 || strings.Count(out, showCursorSequence) != 1 {
			t.Errorf("%q: the screen was not left once, with the cursor shown: %q", chunks, out[max(len(out)-120, 0):])
		}
		if !run.stopped {
			t.Errorf("%q: the interrupt handler was not released", chunks)
		}
	}
}

// The cursor is hidden while a view is on show, and shown at the save prompt
// and on the way out.
func TestStatsScreenHidesTheCursorOnlyWhileViewing(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{dir: t.TempDir()}, "h", "\x1b", "q")
	frames := strings.Split(run.out.String(), clearScreenSequence)[1:]
	if len(frames) != 3 {
		t.Fatalf("%d frames", len(frames))
	}
	frames[len(frames)-1], _, _ = strings.Cut(frames[len(frames)-1], showCursorSequence+leaveAltScreenSequence)
	for i, frame := range frames {
		hidden := strings.HasPrefix(frame, hideCursorSequence)
		shown := strings.Contains(frame, showCursorSequence)
		if !hidden || shown != (i == 1) {
			t.Errorf("frame %d: hidden=%v shown=%v", i, hidden, shown)
		}
	}
}

// A read that fails ends the screen with the terminal restored and the error
// on stderr.
func TestStatsScreenReadErrorRestoresTheTerminal(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{}, "d", string(fakeFailure))
	if run.code != 1 || !strings.Contains(run.stderr, "terminal went away") || run.fake.keyMode() {
		t.Errorf("code %d, stderr %q, key mode %v", run.code, run.stderr, run.fake.keyMode())
	}
	if !strings.HasSuffix(run.out.String(), restoredSuffix) {
		t.Error("the screen was not left")
	}
}

// A panic while a frame is drawn still gives the terminal back.
func TestStatsScreenPanicRestoresTheTerminal(t *testing.T) {
	t.Parallel()
	fake := newFakeKeys("d", "q")
	var out *statsScreenOutput
	captured := make(chan *statsScreenOutput, 1)
	sizes := 0
	func() {
		defer func() {
			if recover() == nil {
				t.Error("no panic")
			}
		}()
		runScreen(t, screenOptions{fake: fake, tweak: func(e *Env) {
			e.TerminalSize = func(w io.Writer) (int, int, bool) {
				if o, ok := w.(*statsScreenOutput); ok {
					select {
					case captured <- o:
					default:
					}
				}
				if sizes++; sizes > 1 && fake.keyMode() {
					panic("drawing failed")
				}
				return 80, 24, true
			}
		}})
	}()
	out = <-captured
	if fake.keyMode() || !strings.HasSuffix(out.String(), restoredSuffix) {
		t.Fatalf("after a panic: key mode %v, modes %v, output ends %q", fake.keyMode(), fake.history(), out.String()[max(len(out.String())-80, 0):])
	}
}

// A signal while the screen waits for a key restores the terminal, the cursor
// and the screen before the process exits, with the code the signal gives.
func TestStatsScreenSignalRestoresTheTerminal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sig  os.Signal
		code int
	}{{os.Interrupt, 130}, {syscall.SIGTERM, 143}, {syscall.SIGHUP, 129}, {syscall.SIGQUIT, 131}} {
		fake := newFakeKeys("d", string(fakeBlock))
		signals := make(chan os.Signal, 1)
		exited := make(chan int, 1)
		var out *statsScreenOutput
		ready := make(chan struct{})
		done := make(chan screenRun, 1)
		go func() {
			done <- runScreen(t, screenOptions{fake: fake, tweak: func(e *Env) {
				e.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
				e.exitProcess = func(code int) {
					if fake.keyMode() || !strings.HasSuffix(outString(out), restoredSuffix) {
						t.Errorf("%v: exit before restoring: modes %v", tc.sig, fake.history())
					}
					exited <- code
				}
				e.TerminalSize = func(w io.Writer) (int, int, bool) {
					if o, ok := w.(*statsScreenOutput); ok && out == nil {
						out = o
						close(ready)
					}
					return 80, 24, true
				}
			}})
		}()
		<-ready
		<-fake.blocked
		signals <- tc.sig
		select {
		case code := <-exited:
			if code != tc.code {
				t.Errorf("%v: exit %d, want %d", tc.sig, code, tc.code)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v: no exit", tc.sig)
		}
		close(fake.unblock)
		run := <-done
		// Closed once by the handler; the screen's own close does nothing.
		if history := fake.history(); !reflect.DeepEqual(history, []string{"keys", "flush", "lines", "release"}) {
			t.Errorf("%v: terminal modes %v", tc.sig, history)
		}
		// Nothing was drawn after the screen was left.
		if s := run.out.String(); !strings.HasSuffix(s, restoredSuffix) {
			t.Errorf("%v: output after leaving: %q", tc.sig, s[max(len(s)-80, 0):])
		}
	}
}

func outString(o *statsScreenOutput) string {
	if o == nil {
		return ""
	}
	return o.String()
}

// Ctrl-Z shows the normal screen and the cursor while the process is stopped,
// and the screen is drawn again, in the same view, when it continues.
func TestStatsScreenSuspendShowsTheNormalScreen(t *testing.T) {
	t.Parallel()
	run := runScreen(t, screenOptions{width: 100, height: 60}, "d", string(fakeSuspend), "q")
	out := run.out.String()
	if strings.Count(out, showCursorSequence+leaveAltScreenSequence) != 2 || strings.Count(out, enterAltScreenSequence) != 2 {
		t.Fatalf("suspend and quit:\n%q", out)
	}
	if !reflect.DeepEqual(run.fake.history()[:5], []string{"keys", "lines", "stop", "keys", "flush"}) {
		t.Errorf("modes %v", run.fake.history())
	}
	// The frame after the stop is the detail view again.
	last := run.frames[len(run.frames)-1]
	checkFrame(t, last, run.expectedLines(pageDetail, 30, 100), 0, 100, 60, barHas("[d detail]"))
}
