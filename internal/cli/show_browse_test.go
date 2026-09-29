package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pagerCall is one page the browser opened.
type pagerCall struct {
	command string
	text    string
}

// browse runs `list` (or args) interactively on input, with a recording
// pager, and returns what was printed.
func browse(t *testing.T, input string, args ...string) (out, errOut string, pages []pagerCall, id string) {
	t.Helper()
	env, _, id := publishedFixture(t)
	stdin := strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&stdout)
	}
	env.RunPager = func(command string, in io.Reader, _, _ io.Writer) error {
		text, err := io.ReadAll(in)
		pages = append(pages, pagerCall{command: command, text: string(text)})
		return err
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	if code := Run(args, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, stderr.String(), stdout.String())
	}
	return stdout.String(), stderr.String(), pages, id
}

const detailsPrompt = "[t] transcript  [Enter/b] back to list  [q] quit"

func TestBrowserBackReturnsToList(t *testing.T) {
	t.Parallel()
	out, _, _, _ := browse(t, "1\nb\n1\n\nq\n")
	if n := strings.Count(out, "Enter number"); n != 3 {
		t.Fatalf("list shown %d times, want 3:\n%s", n, out)
	}
	if n := strings.Count(out, detailsPrompt); n != 2 {
		t.Fatalf("details shown %d times, want 2:\n%s", n, out)
	}
	// Each view starts on a cleared screen instead of below the last one.
	if n := strings.Count(out, clearScreenSequence); n != 5 {
		t.Fatalf("screen cleared %d times, want 5:\n%q", n, out)
	}
}

func TestBrowserQuitFromDetails(t *testing.T) {
	t.Parallel()
	out, _, pages, id := browse(t, "1\nq\n")
	if strings.Count(out, "Enter number") != 1 || strings.Count(out, detailsPrompt) != 1 || len(pages) != 0 {
		t.Fatalf("q did not quit from the details:\n%s", out)
	}
	if !strings.HasPrefix(out, enterAltScreenSequence) || strings.Count(out, leaveAltScreenSequence) != 1 {
		t.Fatalf("alternate screen not entered and left once:\n%q", out)
	}
	_, after, _ := strings.Cut(out, leaveAltScreenSequence)
	if !strings.Contains(after, "ID "+id) || !strings.Contains(after, "--transcript") {
		t.Fatalf("last summary not printed to the normal screen:\n%q", after)
	}
}

func TestBrowserTranscriptOpensPager(t *testing.T) {
	t.Parallel()
	out, _, pages, _ := browse(t, "1\nt\nq\n")
	if len(pages) != 1 {
		t.Fatalf("pager ran %d times:\n%s", len(pages), out)
	}
	// The default pager waits for q even for a short transcript, since the
	// details are redrawn when it exits.
	if pages[0].command != "less -RX -+F" || !strings.Contains(pages[0].text, "visible") {
		t.Fatalf("pager = %q:\n%s", pages[0].command, pages[0].text)
	}
	// After the pager, the details are drawn again.
	if n := strings.Count(out, detailsPrompt); n != 2 || strings.Contains(out, "visible") {
		t.Fatalf("details shown %d times after the pager:\n%s", n, out)
	}
}

func TestBrowserTranscriptWithoutPagerWaits(t *testing.T) {
	t.Parallel()
	out, _, pages, _ := browse(t, "1\nt\nx\n\nq\n", "list", "--no-pager")
	if len(pages) != 0 || !strings.Contains(out, "visible") {
		t.Fatalf("transcript not printed directly (pages=%d):\n%s", len(pages), out)
	}
	if !strings.Contains(out, "[Enter/b] back to details  [q] quit") || !strings.Contains(out, "Enter b (or just Enter) for the details") {
		t.Fatalf("no prompt after the transcript:\n%s", out)
	}
	if n := strings.Count(out, detailsPrompt); n != 2 {
		t.Fatalf("details shown %d times, want 2:\n%s", n, out)
	}
}

func TestBrowserEndOfInputRestoresScreen(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "1\n", "1\nt\n"} {
		out, _, _, _ := browse(t, input, "list", "--no-pager")
		if strings.Count(out, enterAltScreenSequence) != 1 || strings.Count(out, leaveAltScreenSequence) != 1 {
			t.Fatalf("%q: alternate screen not restored:\n%q", input, out)
		}
		_, after, _ := strings.Cut(out, leaveAltScreenSequence)
		if input == "" && after != "" {
			t.Fatalf("%q: printed after leaving with nothing viewed: %q", input, after)
		}
		if input != "" && !strings.Contains(after, "--transcript") {
			t.Fatalf("%q: last summary missing: %q", input, after)
		}
	}
}

func TestBrowserInvalidInputAsksAgain(t *testing.T) {
	t.Parallel()
	out, _, _, _ := browse(t, "1\nzz\nq\n")
	if !strings.Contains(out, "Enter t for the transcript, b (or just Enter) for the list, or q to quit.") || strings.Count(out, detailsPrompt) != 2 {
		t.Fatalf("invalid answer not re-prompted:\n%s", out)
	}
	// Asking again keeps the summary on screen: no redraw.
	if n := strings.Count(out, clearScreenSequence); n != 2 {
		t.Fatalf("screen cleared %d times, want 2:\n%q", n, out)
	}
}

type screenStub struct {
	terminal bool
	signals  chan os.Signal
	stopped  bool
}

func (s *screenStub) isTerminal(any) bool { return s.terminal }

func (s *screenStub) interrupts() (<-chan os.Signal, func()) {
	return s.signals, func() { s.stopped = true }
}

func TestAltScreenOnlyOnATerminal(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	s := enterAltScreen(&out, &screenStub{})
	s.clear()
	s.leave()
	if out.Len() != 0 {
		t.Fatalf("wrote to a non-terminal: %q", out.String())
	}
}

// An interrupt restores the screen before the process exits. While a pager
// owns the terminal, Ctrl-C is left to it and any other signal stops the
// pager first; the browser exits once the pager has.
func TestAltScreenRestoresOnInterrupt(t *testing.T) {
	t.Parallel()
	var out syncBuffer
	stub := &screenStub{terminal: true, signals: make(chan os.Signal, 2)}
	exited := make(chan int, 1)
	s := &altScreen{out: &out, exit: func(code int) { exited <- code }}
	s.start(stub.signals, func() { stub.stopped = true })

	stopped := make(chan struct{})
	s.startPaging(func() { close(stopped) })
	stub.signals <- os.Interrupt // the pager's
	stub.signals <- syscall.SIGTERM
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not stop the pager")
	}
	select {
	case code := <-exited:
		t.Fatalf("exited %d while the pager ran", code)
	default:
	}
	sig := s.endPaging()
	if sig != syscall.SIGTERM || out.String() != enterAltScreenSequence {
		t.Fatalf("pending %v, output %q", sig, out.String())
	}
	s.exitForSignal(sig)
	if code := <-exited; code != 128+int(syscall.SIGTERM) || out.String() != enterAltScreenSequence+leaveAltScreenSequence || !stub.stopped {
		t.Fatalf("exit %d, output %q, stopped %v", code, out.String(), stub.stopped)
	}
	s.leave() // a second leave writes nothing
	if out.String() != enterAltScreenSequence+leaveAltScreenSequence {
		t.Fatalf("output %q", out.String())
	}
}

func TestAltScreenExitsOnInterruptOutsideThePager(t *testing.T) {
	t.Parallel()
	var out syncBuffer
	signals := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	s := &altScreen{out: &out, exit: func(code int) { exited <- code }}
	s.start(signals, func() {})
	signals <- os.Interrupt
	select {
	case code := <-exited:
		if code != 130 || out.String() != enterAltScreenSequence+leaveAltScreenSequence {
			t.Fatalf("exit %d, output %q", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit after Ctrl-C")
	}
}

// Stopping a pager signals it and waits for it to exit.
func TestPagerStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Env{}.runPager(ctx, "sleep 30", strings.NewReader(""), io.Discard, io.Discard)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(pagerStopDelay + 5*time.Second):
		t.Fatal("pager still running after cancel")
	}
}

// For the browser, less is told to wait for q even when the text fits on
// one screen; another pager is run as given.
func TestBrowserPagerCommand(t *testing.T) {
	t.Parallel()
	for pager, want := range map[string]string{
		"":                     "less -RX -+F",
		"less -FR":             "less -FR -+F",
		"/usr/bin/less":        "/usr/bin/less -+F",
		"most":                 "most",
		"less -R | tee /tmp/x": "less -R | tee /tmp/x",
	} {
		env := testEnv(t, t.TempDir(), time.Now())
		var out bytes.Buffer
		env.IsTerminal = func(any) bool { return true }
		env.LookupEnv = func(key string) (string, bool) { return pager, key == "PAGER" && pager != "" }
		var got string
		env.RunPager = func(command string, _ io.Reader, _, _ io.Writer) error { got = command; return nil }
		paged, waited, err := pageText(context.Background(), &out, io.Discard, env, false, true, []byte("x"))
		if err != nil || !paged || got != want || waited != strings.Contains(want, "-+F") {
			t.Errorf("%q: ran %q (waited %v), want %q", pager, got, waited, want)
		}
	}
}

// A pager other than less may return at once, so the browser waits before
// redrawing; after it, the browser enters the alternate screen again in
// case the pager left it.
func TestBrowserWaitsAfterOtherPagers(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	stdin := strings.NewReader("1\nt\n\nq\n")
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	env.LookupEnv = func(key string) (string, bool) { return "most", key == "PAGER" }
	pages := 0
	env.RunPager = func(command string, _ io.Reader, _, _ io.Writer) error {
		pages++
		if command != "most" {
			t.Errorf("pager %q", command)
		}
		return nil
	}
	if code := Run([]string{"list"}, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if pages != 1 || !strings.Contains(out, "[Enter/b] back to details  [q] quit") || strings.Count(out, detailsPrompt) != 2 {
		t.Fatalf("pages=%d:\n%s", pages, out)
	}
	if n := strings.Count(out, enterAltScreenSequence); n != 2 {
		t.Fatalf("alternate screen entered %d times, want 2:\n%q", n, out)
	}
}
