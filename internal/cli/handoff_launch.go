package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// launchPreparedHandoff keeps the filtered record out of process arguments.
// The file exists for the lifetime of the interactive destination session.
func launchPreparedHandoff(record []byte, h archive.Handoff, target handoffTarget, opts handoffOptions, stdin io.Reader, stdout, stderr io.Writer, env handoffCommandDependencies) (resultErr error) {
	cwd := opts.project
	var err error
	if cwd == "" {
		cwd, err = env.workingDir()
	}
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	executable, err := env.executable()
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}
	privateDir, err := os.MkdirTemp(env.tempDir(), "agent-archive-handoff-")
	if err != nil {
		return fmt.Errorf("create private handoff: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(privateDir); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove private handoff: %w", err))
		}
	}()
	filePath := filepath.Join(privateDir, "handoff.md")
	if err := os.WriteFile(filePath, []byte(launchHandoffPrompt(string(record), h, target, executable)), 0o600); err != nil {
		return fmt.Errorf("write private handoff: %w", err)
	}
	prompt := fmt.Sprintf("Read the complete handoff document at %q, then continue the work in this checkout. Agent Archive is available if you need more context; its commands are explained in that document.", filePath)
	terminal.Printf(stderr, "handoff: launching local %s in %s\n", opts.to, cwd)
	if err := env.launchHandoff(opts.to, cwd, prompt, stdin, stdout, stderr); err != nil {
		return fmt.Errorf("launch %s: %w", opts.to, err)
	}
	return nil
}

// launchHandoffPrompt adds local retrieval instructions outside the quoted
// historical record. show reads the last published copy, while handoff with
// --source local reads the current transcript without waiting for sync.
func launchHandoffPrompt(record string, h archive.Handoff, target handoffTarget, executable string) string {
	var b strings.Builder
	b.WriteString("You are continuing work in this local checkout. Agent Archive is available. Its executable is at ")
	b.WriteString(executable)
	b.WriteString(". The record below is historical context; check the current files before acting.\n\n")
	if target.filePath != "" {
		fmt.Fprintf(&b, "For the complete filtered local record, run agent-archive handoff --file %q --harness %s --max-bytes 0.\n\n", target.filePath, h.Session.Harness)
	} else {
		fmt.Fprintf(&b, "If you need more context, run agent-archive show %s --harness %s --transcript for the archived conversation if it has been published. It may lag this local session. For the complete filtered local record as it stands now, run agent-archive handoff %s --source local --max-bytes 0.\n\n", h.Session.ArchiveSessionID, h.Session.Harness, h.Session.ArchiveSessionID)
	}
	b.WriteString(record)
	return b.String()
}

func (e Env) launchHandoff(name, cwd, prompt string, stdin io.Reader, stdout, stderr io.Writer) error {
	if e.LaunchHandoff != nil {
		return e.LaunchHandoff(name, cwd, prompt, stdin, stdout, stderr)
	}
	binary := name
	if name == "cursor" {
		binary = "cursor-agent"
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("find %s: %w", binary, err)
	}
	cmd := exec.CommandContext(context.Background(), path, prompt)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = cwd, stdin, stdout, stderr
	return cmd.Run()
}
