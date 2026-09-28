package cli

import (
	"bytes"
	"context"
	"io"
	"os/exec"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// withPager writes through write. When stdout is a terminal and paging is
// not disabled, the written bytes go to $AGENT_ARCHIVE_PAGER, else $PAGER,
// else `less -FRX`, so a long listing can be scrolled and quit with q.
// Piped or redirected stdout is never paged; a spawn failure falls back to
// writing stdout directly after a stderr warning.
func withPager(stdout, stderr io.Writer, env Env, noPager bool, write func(io.Writer) error) error {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return err
	}
	command, page := resolvePagerCommand(env, noPager, stdout)
	if !page {
		_, err := io.Copy(stdout, &buf)
		return err
	}
	if err := env.runPager(command, &buf, stdout, stderr); err != nil {
		terminal.Printf(stderr, "agent-archive: warning: pager %q failed (%v); printing directly\n", command, err)
		_, copyErr := io.Copy(stdout, bytes.NewReader(buf.Bytes()))
		return copyErr
	}
	return nil
}

// resolvePagerCommand chooses the pager command. An empty
// AGENT_ARCHIVE_PAGER or PAGER, or the value "cat", disables paging, as does
// --no-pager or a non-terminal stdout.
func resolvePagerCommand(env Env, noPager bool, stdout io.Writer) (command string, page bool) {
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
	return "less -FRX", true
}

func (e Env) runPager(command string, stdin io.Reader, stdout, stderr io.Writer) error {
	if e.RunPager != nil {
		return e.RunPager(command, stdin, stdout, stderr)
	}
	cmd := exec.CommandContext(context.Background(), "sh", "-c", command)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
