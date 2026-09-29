package cli

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// withPager writes through write. When stdout is a terminal and paging is
// not disabled, the written bytes go to $AGENT_ARCHIVE_PAGER, else $PAGER,
// else `less -FRX`, so a long listing can be scrolled and quit with q.
// Piped or redirected stdout is never paged; a spawn failure falls back to
// writing stdout directly after a stderr warning.
func withPager(stdout, stderr io.Writer, env pagerDependencies, noPager bool, write func(io.Writer) error) error {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return err
	}
	_, _, err := pageText(context.Background(), stdout, stderr, env, noPager, false, buf.Bytes())
	return err
}

// defaultPager is the pager when neither AGENT_ARCHIVE_PAGER nor PAGER is
// set. -F quits at once when the text fits on one screen; -X leaves the
// text on the screen after quitting.
const defaultPager = "less -FRX"

// pageText writes text through the pager withPager would choose, and
// reports whether a pager showed it. Cancelling ctx stops the pager.
//
// stayOpen is for the session browser, which redraws the screen when the
// pager exits. less is then told not to quit at once when the text fits on
// one screen (-+F overrides an -F from the command or $LESS), and waited
// reports that the pager is known to wait for the user; another pager may
// have returned at once, so the browser waits itself.
func pageText(ctx context.Context, stdout, stderr io.Writer, env pagerDependencies, noPager, stayOpen bool, text []byte) (paged, waited bool, err error) {
	command, page := resolvePagerCommand(env, noPager, stdout)
	if !page {
		_, err := stdout.Write(text)
		return false, false, err
	}
	if stayOpen && isLess(command) {
		if command == defaultPager {
			command = "less -RX"
		}
		command += " -+F"
		waited = true
	}
	if err := env.runPager(ctx, command, bytes.NewReader(text), stdout, stderr); err != nil {
		if ctx.Err() != nil {
			return true, waited, nil
		}
		terminal.Printf(stderr, "agent-archive: warning: pager %q failed (%v); printing directly\n", command, err)
		_, copyErr := stdout.Write(text)
		return false, false, copyErr
	}
	return true, waited, nil
}

// isLess reports whether command runs less by itself, so options can be
// appended to it.
func isLess(command string) bool {
	if strings.ContainsAny(command, "|;&<>`$()") {
		return false
	}
	fields := strings.Fields(command)
	return len(fields) > 0 && filepath.Base(fields[0]) == "less"
}

// resolvePagerCommand chooses the pager command. An empty
// AGENT_ARCHIVE_PAGER or PAGER, or the value "cat", disables paging, as does
// --no-pager or a non-terminal stdout.
func resolvePagerCommand(env pagerDependencies, noPager bool, stdout io.Writer) (command string, page bool) {
	if noPager || !env.isTerminal(stdout) {
		return "", false
	}
	if value, set := env.lookupEnv("AGENT_ARCHIVE_PAGER"); set {
		if value == "" || value == "cat" {
			return "", false
		}
		return value, true
	}
	if value, set := env.lookupEnv("PAGER"); set {
		if value == "" || value == "cat" {
			return "", false
		}
		return value, true
	}
	return defaultPager, true
}

// pagerStopDelay is how long a pager asked to stop may take before it is
// killed.
const pagerStopDelay = 3 * time.Second

// runPager runs command through sh. Cancelling ctx sends the pager SIGTERM
// (sh execs a lone command, so the signal reaches the pager itself) and
// waits for it to exit, killing it after pagerStopDelay, so no pager is
// left behind on the terminal.
func (e Env) runPager(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	if e.RunPager != nil {
		return e.RunPager(command, stdin, stdout, stderr)
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = pagerStopDelay
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
