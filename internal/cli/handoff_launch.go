package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// launchPreparedHandoff starts dest with the filtered record: in this
// terminal when here is set, returning when the agent exits, else in a new
// terminal window or tab, returning once it opens.
func launchPreparedHandoff(record []byte, h archive.Handoff, target handoffTarget, dest handoffDestination, here bool, opts handoffOptions, home string, stdin, answers io.Reader, stdout, stderr io.Writer, env handoffLaunchDependencies) error {
	dir, err := launchDir(opts, env)
	if err != nil {
		return err
	}
	spec, err := prepareLaunch(record, h, target, dest, dir, opts, home, stdin, answers, stderr, env)
	if err != nil {
		return err
	}
	if here {
		terminal.Printf(stderr, "handoff: launching local %s in %s\n", dest, spec.Dir)
		finishTraceNow()
		if err := env.launchHandoff(spec, stdin, stdout, stderr); err != nil {
			return fmt.Errorf("launch %s: %w", dest, err)
		}
		return nil
	}
	// The launcher script goes beside the launch copy, in a directory only
	// this user can write to (termlaunch refuses any other). The new window
	// starts from the terminal's environment, not spec.Env, so the calling
	// agent's session variables are unset there.
	where, err := env.openTerminal(termlaunch.Spec{Dir: spec.Dir, Argv: append([]string{spec.Binary}, spec.Args...),
		Unset: handoffSessionEnv, ScriptDir: filepath.Dir(spec.HandoffFile)})
	if err != nil {
		// termlaunch.ErrNoTerminal's message ends with the command to run.
		return fmt.Errorf("open %s: %w", dest, err)
	}
	terminal.Printf(stderr, "handoff: opened %s in %s\n", dest, where)
	return nil
}

// launchDir is the absolute directory the agent starts in: --project, else
// the working directory.
func launchDir(opts handoffOptions, env workingDirDependencies) (string, error) {
	dir := opts.project
	var err error
	if dir == "" {
		dir, err = env.workingDir()
	}
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	return dir, nil
}

// prepareLaunch writes the launch copy of the handoff and builds the command
// that starts dest in dir, or in a new worktree of it (prepareLaunchDir). The
// record stays out of process arguments: the agent's prompt only names the
// file. The arguments after `--` follow the configured arguments for dest.
func prepareLaunch(record []byte, h archive.Handoff, target handoffTarget, dest handoffDestination, dir string, opts handoffOptions, home string, stdin, answers io.Reader, stderr io.Writer, env handoffLaunchDependencies) (launchSpec, error) {
	executable, err := env.executable()
	if err != nil {
		return launchSpec{}, fmt.Errorf("executable: %w", err)
	}
	// `--file` works before setup, when there is no configuration.
	cfg, _, err := config.Load(home)
	if err != nil {
		return launchSpec{}, fmt.Errorf("load config: %w", err)
	}
	content := launchHandoffPrompt(string(record), h, target, executable)
	path, err := writeLaunchHandoff(home, env.tempDir(), target, []byte(content), env.now())
	if err != nil {
		return launchSpec{}, err
	}
	prompt := fmt.Sprintf("Read the complete handoff document at %q, then continue the work in this checkout. Agent Archive is available if you need more context; its commands are explained in that document.", path)
	args := slices.Concat(cfg.Handoff.Args[string(dest)], opts.agentArgs)
	// Build the command before choosing the directory, so a missing agent
	// or bad arguments fail before a worktree is created.
	spec, err := buildLaunchSpec(dest, prompt, path, dir, args, env)
	if err == nil {
		var launch string
		if launch, err = prepareLaunchDir(env, opts, target, dir, stdin, answers, stderr); err == nil && launch != dir {
			spec, err = buildLaunchSpec(dest, prompt, path, launch, args, env)
		}
	}
	if err != nil {
		// Nothing will read it. Its directory holds only this copy.
		_ = os.RemoveAll(filepath.Dir(path))
		return launchSpec{}, err
	}
	return spec, nil
}

// launchHandoffName is the file a launch copy is saved as, alone in a
// directory of its own so Claude Code's --add-dir exposes nothing else.
const launchHandoffName = "handoff.md"

// writeLaunchHandoff saves the document a launched agent reads, in a new
// 0700 directory: <home>/handoffs/launch-<name>-<unix>-<random>/. It is kept after the
// agent exits, so a resumed session can read it again, until pruneHandoffs
// removes it after handoffMaxAge. Without a data directory (`--file` before
// setup) the directory is a new private one under tempDir instead, also
// kept (a resumed session may read it again) for the system to clear.
func writeLaunchHandoff(home, tempDir string, target handoffTarget, content []byte, now time.Time) (string, error) {
	if _, err := os.Stat(home); err != nil {
		dir, err := os.MkdirTemp(tempDir, "agent-archive-handoff-")
		if err != nil {
			return "", fmt.Errorf("create private handoff: %w", err)
		}
		path := filepath.Join(dir, launchHandoffName)
		if err := writeNewFile(path, content); err != nil {
			return "", fmt.Errorf("write private handoff: %w", err)
		}
		return path, nil
	}
	parent := filepath.Join(home, handoffDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("write handoff: %w", err)
	}
	// MkdirTemp adds a random suffix, so two launches of one session in
	// the same second get separate directories, and it never reuses a name
	// already there (a symlink included).
	dir, err := os.MkdirTemp(parent, fmt.Sprintf("%s%s-%d-*", launchHandoffPrefix, handoffFileName(target.bundle), now.Unix()))
	if err != nil {
		return "", fmt.Errorf("write handoff: %w", err)
	}
	path := filepath.Join(dir, launchHandoffName)
	if err := writeNewFile(path, content); err != nil {
		return "", fmt.Errorf("write handoff: %w", err)
	}
	return path, nil
}

// writeNewFile creates path with mode 0600, failing if anything is already
// there, so a launched agent never reads a file someone else placed.
func writeNewFile(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// launchHandoffPrompt adds local retrieval instructions outside the quoted
// historical record. show reads the last published copy, while handoff with
// --source local reads the current transcript without waiting for sync. A
// session handed off from the archive has no local transcript to point to.
func launchHandoffPrompt(record string, h archive.Handoff, target handoffTarget, executable string) string {
	var b strings.Builder
	b.WriteString("You are continuing work in this local checkout. Agent Archive is available. Its executable is at ")
	b.WriteString(executable)
	b.WriteString(". The record below is historical context; check the current files before acting.\n\n")
	switch {
	case target.filePath != "":
		fmt.Fprintf(&b, "For the complete filtered local record, run agent-archive handoff --file %q --harness %s --max-bytes 0.\n\n", target.filePath, h.Session.Harness)
	case target.source == "archive":
		fmt.Fprintf(&b, "If you need more context, run agent-archive show %s --harness %s --transcript for the archived conversation, or agent-archive handoff %s --harness %s --max-bytes 0 for the complete filtered record.\n\n", h.Session.ArchiveSessionID, h.Session.Harness, h.Session.ArchiveSessionID, h.Session.Harness)
	default:
		fmt.Fprintf(&b, "If you need more context, run agent-archive show %s --harness %s --transcript for the archived conversation if it has been published. It may lag this local session. For the complete filtered local record as it stands now, run agent-archive handoff %s --source local --max-bytes 0.\n\n", h.Session.ArchiveSessionID, h.Session.Harness, h.Session.ArchiveSessionID)
	}
	b.WriteString(record)
	return b.String()
}

func (e Env) launchHandoff(spec launchSpec, stdin io.Reader, stdout, stderr io.Writer) error {
	if e.LaunchHandoff != nil {
		return e.LaunchHandoff(spec, stdin, stdout, stderr)
	}
	cmd := exec.CommandContext(context.Background(), spec.Binary, spec.Args...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

func (e Env) lookPath(name string) (string, error) {
	if e.LookPath != nil {
		return e.LookPath(name)
	}
	return exec.LookPath(name)
}

func (e Env) environ() []string {
	if e.Environ != nil {
		return e.Environ()
	}
	return os.Environ()
}
