//go:build darwin || linux

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// This child is only launched by the test below: it registers the signal set
// every command of Env uses by default, says so, and exits as the first signal
// it gets would end a command: with the shell's status for it.
func TestInterruptsChild(t *testing.T) {
	t.Parallel()
	if os.Getenv("ARCHIVE_INTERRUPTS_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	signals, stop := Env{}.interrupts()
	if _, err := os.Stdout.WriteString("ready\n"); err != nil {
		t.Fatal(err)
	}
	code := 99
	select {
	case sig := <-signals:
		code = signalExitCode(sig)
	case <-time.After(30 * time.Second):
	}
	stop()
	os.Exit(code)
}

// The default signal set is the one the commands restore the terminal for:
// Ctrl-C, SIGTERM, SIGHUP and SIGQUIT each reach it (a kill -QUIT from outside
// used to dump goroutines and leave a screen's terminal raw), and the exit
// status is the shell's for the signal. It runs in a process of its own, since
// a signal is the whole process's, and no other test may see it.
func TestDefaultInterruptsCoverQuit(t *testing.T) {
	t.Parallel()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sig  syscall.Signal
		code int
	}{{syscall.SIGINT, 130}, {syscall.SIGTERM, 143}, {syscall.SIGHUP, 129}, {syscall.SIGQUIT, 131}} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, binary, "-test.run=^TestInterruptsChild$")
		cmd.Env = append(os.Environ(), "ARCHIVE_INTERRUPTS_TEST_CHILD=1")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("%v: the child said %q (%v); stderr %q", tc.sig, line, err, stderr.String())
		}
		if err := cmd.Process.Signal(tc.sig); err != nil {
			t.Fatal(err)
		}
		err = cmd.Wait()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != tc.code {
			t.Errorf("%v: %v, want exit status %d", tc.sig, err, tc.code)
		}
		if bytes.Contains(stderr.Bytes(), []byte("goroutine ")) || bytes.Contains(stderr.Bytes(), []byte("SIGQUIT")) {
			t.Errorf("%v: the child dumped goroutines: %.300s", tc.sig, stderr.String())
		}
	}
}
