// Package termlaunch runs a command in a new terminal window or tab: a tmux
// window inside tmux, else on macOS an iTerm2 or Ghostty tab, or a new
// Terminal.app window.
//
// The command never passes through AppleScript or tmux string
// interpolation. Open writes a private /bin/sh launcher script whose every
// word is single-quoted, and hands the terminal only that script's path:
// tmux gets it single-quoted, and AppleScript receives it as a run-handler
// argument and quotes it with `quoted form of`, so no value is ever spliced
// into AppleScript source.
package termlaunch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ErrNoTerminal reports that there is no terminal Open knows how to open a
// window in: not inside tmux, and not on macOS. The wrapping error names the
// command to run by hand.
var ErrNoTerminal = errors.New("no terminal to open a new window in")

// Spec is what to run in the new window.
type Spec struct {
	// Dir is the absolute directory the command runs in.
	Dir string
	// Argv is the command, an absolute path, and its arguments, passed to
	// it byte for byte.
	Argv []string
	// Unset names environment variables the command must not inherit from
	// the terminal's shell.
	Unset []string
	// ScriptDir is the absolute, private (0700) directory the launcher
	// script is written to. The script removes itself when it starts.
	ScriptDir string
}

// Environment is what Open reads and runs; tests replace it.
type Environment struct {
	GOOS      string
	LookupEnv func(string) (string, bool)
	Run       func(ctx context.Context, name string, args ...string) error
}

// DefaultEnvironment is this process's operating system and environment,
// running commands for real.
func DefaultEnvironment() Environment {
	return Environment{GOOS: runtime.GOOS, LookupEnv: os.LookupEnv, Run: run}
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// Open starts spec in a new terminal window or tab and returns once the
// terminal has accepted it, with a description of where it opened ("a new
// tmux window"). It does not wait for the command.
func Open(ctx context.Context, spec Spec, env Environment) (where string, err error) {
	if err := validate(spec); err != nil {
		return "", err
	}
	tmux := false
	if v, ok := env.LookupEnv("TMUX"); ok && v != "" {
		tmux = true
	}
	if !tmux && env.GOOS != "darwin" {
		return "", fmt.Errorf("%w; run it by hand:\n  %s", ErrNoTerminal, handCommand(spec))
	}
	path, err := writeScript(spec)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if tmux {
		// tmux expands formats in -c (#(...) would run a command), not in
		// the shell command; ## is a literal #.
		dir := strings.ReplaceAll(spec.Dir, "#", "##")
		return "a new tmux window", env.Run(ctx, "tmux", "new-window", "-c", dir, shellQuote(path))
	}
	program, _ := env.LookupEnv("TERM_PROGRAM")
	switch termProgram(program) {
	case termITerm:
		return "a new iTerm2 tab", env.Run(ctx, "osascript", osascriptArgs(iTermScript, path)...)
	case termGhostty:
		if err := env.Run(ctx, "osascript", osascriptArgs(ghosttyScript, path, spec.Dir)...); err == nil {
			return "a new Ghostty tab", nil
		}
		// Ghostty before 1.3 has no AppleScript dictionary.
	}
	return "a new Terminal window", env.Run(ctx, "osascript", osascriptArgs(terminalScript, path)...)
}

// termProgram is a $TERM_PROGRAM value with a terminal of its own; any other
// value on macOS gets Terminal.app.
type termProgram string

const (
	termITerm   termProgram = "iTerm.app"
	termGhostty termProgram = "ghostty"
)

// The AppleScript run handlers. item 1 of argv is the launcher script's
// path; Ghostty's also takes the working directory as item 2.
var (
	iTermScript = []string{
		"on run argv",
		"set cmd to quoted form of (item 1 of argv)",
		`tell application "iTerm2"`,
		"if current window is missing value then",
		"create window with default profile command cmd",
		"else",
		"tell current window to create tab with default profile command cmd",
		"end if",
		"activate",
		"end tell",
		"end run",
	}
	ghosttyScript = []string{
		"on run argv",
		`tell application "Ghostty"`,
		// Activated first so the tab is the last step: an error after it
		// opened would fall back to Terminal.app and launch twice.
		"activate",
		"set cfg to new surface configuration",
		"set initial working directory of cfg to (item 2 of argv)",
		"set command of cfg to quoted form of (item 1 of argv)",
		"if (count of windows) is 0 then",
		"new window with configuration cfg",
		"else",
		"new tab in front window with configuration cfg",
		"end if",
		"end tell",
		"end run",
	}
	terminalScript = []string{
		"on run argv",
		`tell application "Terminal"`,
		"do script (quoted form of (item 1 of argv))",
		"activate",
		"end tell",
		"end run",
	}
)

// osascriptArgs is `-e line` for each line, then the run handler's arguments.
func osascriptArgs(lines []string, argv ...string) []string {
	args := make([]string, 0, 2*len(lines)+len(argv))
	for _, line := range lines {
		args = append(args, "-e", line)
	}
	return append(args, argv...)
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validate(spec Spec) error {
	if len(spec.Argv) == 0 {
		return errors.New("termlaunch: no command to run")
	}
	if !filepath.IsAbs(spec.Argv[0]) {
		// The new window's PATH is the terminal's, not this process's.
		return fmt.Errorf("termlaunch: command %q is not an absolute path", spec.Argv[0])
	}
	if !filepath.IsAbs(spec.Dir) {
		return fmt.Errorf("termlaunch: directory %q is not absolute", spec.Dir)
	}
	if !filepath.IsAbs(spec.ScriptDir) {
		return fmt.Errorf("termlaunch: script directory %q is not absolute", spec.ScriptDir)
	}
	words := append([]string{spec.Dir, spec.ScriptDir}, spec.Argv...)
	for _, w := range words {
		if strings.IndexByte(w, 0) >= 0 {
			return errors.New("termlaunch: a NUL byte cannot pass through a command line")
		}
	}
	for _, name := range spec.Unset {
		if !envName.MatchString(name) {
			return fmt.Errorf("termlaunch: %q is not an environment variable name", name)
		}
	}
	return nil
}

// writeScript creates the launcher script under a random name, never
// following or replacing an existing file.
func writeScript(spec Spec) (string, error) {
	// Anyone who can write to the directory could swap the script between
	// writing and running it.
	info, err := os.Stat(spec.ScriptDir)
	if err != nil {
		return "", fmt.Errorf("termlaunch: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("termlaunch: script directory %s is not a directory only its owner can write to", spec.ScriptDir)
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	path := filepath.Join(spec.ScriptDir, "launch-"+hex.EncodeToString(b[:])+".sh")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", fmt.Errorf("termlaunch: %w", err)
	}
	// The umask must not strip the execute bit the terminal needs.
	err = f.Chmod(0o700)
	if err == nil {
		_, err = f.WriteString(script(spec, path))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("termlaunch: %w", err)
	}
	return path, nil
}

// script is the launcher's text. It removes itself first (the shell keeps
// reading from the open file), and on a failure waits for Enter so the
// message stays on screen.
func script(spec Spec, self string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Written by agent-archive to open a handoff in a new terminal.\n")
	b.WriteString("rm -f -- " + shellQuote(self) + "\n")
	if len(spec.Unset) > 0 {
		b.WriteString("unset " + strings.Join(spec.Unset, " ") + "\n")
	}
	b.WriteString("cd -- " + shellQuote(spec.Dir) + " || { printf 'Press Enter to close. '; read -r _; exit 1; }\n")
	b.WriteString(quoteWords(spec.Argv) + "\n")
	b.WriteString("status=$?\n")
	b.WriteString("if [ \"$status\" -ne 0 ]; then\n")
	b.WriteString("  printf '\\n%s exited with status %s. Press Enter to close. ' " + shellQuote(spec.Argv[0]) + " \"$status\"\n")
	b.WriteString("  read -r _\n")
	b.WriteString("fi\n")
	b.WriteString("exit \"$status\"\n")
	return b.String()
}

// handCommand is the command line a person can paste into a shell (Dir is
// absolute, so cd needs no --).
func handCommand(spec Spec) string {
	var b strings.Builder
	b.WriteString("cd " + shellQuote(spec.Dir) + " && ")
	if len(spec.Unset) > 0 {
		b.WriteString("env")
		for _, name := range spec.Unset {
			b.WriteString(" -u " + name)
		}
		b.WriteString(" ")
	}
	b.WriteString(quoteWords(spec.Argv))
	return b.String()
}

func quoteWords(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}

// shellQuote single-quotes s for a POSIX shell; inside single quotes only
// the quote itself needs escaping.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
