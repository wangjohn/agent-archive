package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A page h saved is reported after the screen is left even when a signal ends
// the screen, as it is after a quit, so the path stays in the scrollback; and
// only once, however the two ways out race. Nothing saved, nothing printed.
func TestStatsScreenReportsSavedPagesOnASignal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		sig   os.Signal
		code  int
		saved bool
	}{
		{"interrupt", os.Interrupt, 130, true},
		{"terminate", syscall.SIGTERM, 143, true},
		{"hang up", syscall.SIGHUP, 129, true},
		{"quit", syscall.SIGQUIT, 131, true},
		{"quit, nothing saved", syscall.SIGQUIT, 131, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "out.html")
			wrote := "Wrote " + path + "\n"
			chunks := []string{"d", string(fakeBlock)}
			wantTail := restoredSuffix
			if tc.saved {
				chunks = []string{"h", "out.html", "\r", string(fakeBlock)}
				wantTail += wrote
			}
			fake := newFakeKeys(chunks...)
			signals := make(chan os.Signal, 1)
			exited := make(chan int, 1)
			var out *statsScreenOutput
			ready := make(chan struct{})
			done := make(chan screenRun, 1)
			go func() {
				done <- runScreen(t, screenOptions{width: 100, height: 30, dir: dir, fake: fake, tweak: func(e *Env) {
					e.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
					e.exitProcess = func(code int) {
						// The process exits only once the screen is left and
						// the path is on the normal screen.
						if got := outString(out); !strings.HasSuffix(got, wantTail) {
							t.Errorf("exit before the screen was restored and the path printed: %q", got[max(len(got)-120, 0):])
						}
						exited <- code
					}
					e.TerminalSize = func(w io.Writer) (int, int, bool) {
						if o, ok := w.(*statsScreenOutput); ok && out == nil {
							out = o
							close(ready)
						}
						return 100, 30, true
					}
				}})
			}()
			<-ready
			<-fake.blocked
			signals <- tc.sig
			select {
			case code := <-exited:
				if code != tc.code {
					t.Errorf("exit %d, want %d", code, tc.code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no exit")
			}
			close(fake.unblock)
			got := (<-done).out.String()
			// The exit stub returns, so the screen then ends as a quit does:
			// the path must not be printed a second time.
			wantPaths := 0
			if tc.saved {
				wantPaths = 1
			}
			if n := strings.Count(got, "Wrote "); n != wantPaths {
				t.Errorf("%d paths printed, want %d: %q", n, wantPaths, got[max(len(got)-200, 0):])
			}
			if _, err := os.Stat(path); tc.saved && err != nil {
				t.Error(err)
			}
		})
	}
}
