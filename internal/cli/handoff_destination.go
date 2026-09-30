package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/terminal"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// handoffAction is what the destination prompt was answered with.
type handoffAction int

const (
	handoffLaunch handoffAction = iota
	handoffPrint
	handoffCopy
	handoffWrite
	handoffQuit
)

// handoffChoice is an answer to the destination prompt: an agent to launch,
// or what to do with the handoff instead.
type handoffChoice struct {
	action handoffAction
	// dest is the agent to launch, for handoffLaunch.
	dest handoffDestination
}

// handoffLetters are the prompt's choices other than an agent, in the order
// it lists them. Each is answered by its key or its word.
var handoffLetters = []struct {
	key    string
	word   string
	label  string
	action handoffAction
}{
	{"p", "print", "print", handoffPrint},
	{"c", "copy", "copy to the clipboard", handoffCopy},
	{"w", "write", "write to a file", handoffWrite},
	{"q", "quit", "quit", handoffQuit},
}

// handoffDestinations lists the agents in the order the prompt offers them
// after the default.
var handoffDestinations = []handoffDestination{handoffDestinationClaude, handoffDestinationCodex, handoffDestinationCursor}

var handoffDestinationLabels = map[handoffDestination]string{
	handoffDestinationClaude: "Claude Code",
	handoffDestinationCodex:  "Codex",
	handoffDestinationCursor: "Cursor",
}

// handoffDefaultDestination is where a session goes by default when the
// configuration names nothing: to another agent than the one it came from.
var handoffDefaultDestination = map[string]handoffDestination{
	archive.HarnessClaude: handoffDestinationCodex,
	archive.HarnessCodex:  handoffDestinationClaude,
	archive.HarnessCursor: handoffDestinationClaude,
}

// offersDestinations reports whether the command asks where the handoff
// goes: on a terminal, when nothing already says. A pipe, --output, and
// JSON print as they always have, so `codex "$(agent-archive handoff
// --latest)"` still works. So does --no-preamble: a launched agent needs the
// preamble (--to refuses it), so the only use left for it is printing.
func offersDestinations(opts handoffOptions, interactive bool) bool {
	return interactive && opts.to == "" && opts.output == "" && opts.format == "markdown" && !opts.noPreamble
}

// installedDestinations returns the agents whose CLI is on PATH, in
// handoffDestinations order.
func installedDestinations(env launchSpecDependencies) []handoffDestination {
	var installed []handoffDestination
	for _, dest := range handoffDestinations {
		for _, name := range agentCommands[dest].binaries {
			if _, err := env.lookPath(name); err == nil {
				installed = append(installed, dest)
				break
			}
		}
	}
	return installed
}

// defaultDestination is the installed agent Enter chooses for a session
// from source: the configured one, else another agent than source, else
// the first installed. It is empty when none is installed.
func defaultDestination(installed []handoffDestination, source string, cfg config.HandoffConfig) handoffDestination {
	for _, dest := range []handoffDestination{handoffDestination(cfg.DefaultTo[source]), handoffDefaultDestination[source]} {
		if dest != "" && slices.Contains(installed, dest) {
			return dest
		}
	}
	if len(installed) == 0 {
		return ""
	}
	return installed[0]
}

// chooseDestination asks where the handoff goes: one of the installed
// agents, numbered with def first, or print, copy, write to a file, or quit.
// Enter takes def, or print when no agent is installed. Input ending is
// quitting, as in the session picker.
func chooseDestination(p *prompter, installed []handoffDestination, def handoffDestination) (handoffChoice, error) {
	agents := installed
	if i := slices.Index(installed, def); i > 0 {
		agents = slices.Concat([]handoffDestination{def}, installed[:i], installed[i+1:])
	}
	p.heading("Continue in:")
	for i, dest := range agents {
		label := handoffDestinationLabels[dest]
		if dest == def {
			label += " (default)"
		}
		terminal.Printf(p.out, "  %d) %s\n", i+1, label)
	}
	for _, l := range handoffLetters {
		terminal.Printf(p.out, "  %s) %s\n", l.key, l.label)
	}
	label, defKey := "Enter p, c, w, or q", "p"
	switch {
	case len(agents) == 1:
		label, defKey = "Enter 1, p, c, w, or q", "1"
	case len(agents) > 1:
		label, defKey = fmt.Sprintf("Enter 1-%d, p, c, w, or q", len(agents)), "1"
	}
	for {
		answer, err := p.choose(label, defKey)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return handoffChoice{action: handoffQuit}, nil
			}
			return handoffChoice{}, err
		}
		answer = strings.ToLower(answer)
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(agents) {
			return handoffChoice{action: handoffLaunch, dest: agents[n-1]}, nil
		}
		// An agent's name works as well as its number.
		if slices.Contains(agents, handoffDestination(answer)) {
			return handoffChoice{action: handoffLaunch, dest: handoffDestination(answer)}, nil
		}
		// So does a letter's word.
		for _, l := range handoffLetters {
			if answer == l.key || answer == l.word {
				return handoffChoice{action: l.action}, nil
			}
		}
		terminal.Printf(p.out, "%s.\n", label)
	}
}

// askHandoffDestination offers the agents installed here, defaulting by the
// source session's harness and config.json's handoff.default_to.
func askHandoffDestination(p *prompter, h archive.Handoff, home string, env handoffDestinationDependencies) (handoffChoice, error) {
	// `--file` works before setup, when there is no configuration.
	cfg, _, err := config.Load(home)
	if err != nil {
		return handoffChoice{}, fmt.Errorf("load config: %w", err)
	}
	installed := installedDestinations(env)
	return chooseDestination(p, installed, defaultDestination(installed, archive.CanonicalHarness(h.Session.Harness), cfg.Handoff))
}

// deliverHandoff carries out a choice other than launching: print it (paged
// when longer than the screen, as show is), copy it, or write it to a file.
func deliverHandoff(choice handoffChoice, p *prompter, rendered []byte, target handoffTarget, opts handoffOptions, stdout, stderr io.Writer, env handoffDestinationDependencies) error {
	switch choice.action {
	case handoffPrint:
		_, _, err := pageText(context.Background(), stdout, stderr, env, false, false, rendered)
		return err
	case handoffCopy:
		if err := env.clipboard(rendered); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		terminal.Printf(stderr, "handoff: copied %d bytes\n", len(rendered))
		return nil
	case handoffWrite:
		// The default file goes where an agent would have started.
		dir, err := launchDir(opts, env)
		if err != nil {
			return err
		}
		return writeHandoffChoice(p, rendered, target, dir, stderr, env)
	case handoffLaunch, handoffQuit:
		// Launching is the caller's; quitting does nothing.
	}
	return nil
}

// writeHandoffChoice asks for a path and writes the handoff there with mode
// 0600, replacing an existing file only when told to. The default is in dir.
// A leading ~/ is the home directory, as in a shell; ~user is left as
// typed. q cancels (./q is a file named q). A write that fails is reported
// and asked again, with no default: Enter then cancels too.
func writeHandoffChoice(p *prompter, rendered []byte, target handoffTarget, dir string, stderr io.Writer, env handoffDestinationDependencies) error {
	def := filepath.Join(dir, "handoff-"+shortSessionID(handoffFileName(target.bundle))+".md")
	label := "Write to"
	for {
		path, err := p.withDefault(label, def)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if path == "q" || (def == "" && path == "") {
			return nil
		}
		if path == "~" || strings.HasPrefix(path, "~/") {
			home, err := env.userHomeDir()
			if err != nil {
				return fmt.Errorf("home directory: %w", err)
			}
			path = filepath.Join(home, path[1:])
		}
		force := false
		info, statErr := os.Lstat(path)
		if statErr == nil && info.IsDir() {
			// Replacing it could only fail after asking.
			terminal.Printf(stderr, "agent-archive: handoff: %s is a directory\n", path)
			label, def = "Write to (Enter to cancel)", ""
			continue
		}
		if statErr == nil {
			replace, err := p.yesNo(path+" already exists. Replace it?", false)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if !replace {
				continue
			}
			force = true
		}
		if err := writeHandoffOutput(path, rendered, force); err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			label, def = "Write to (Enter to cancel)", ""
			continue
		}
		terminal.Printf(stderr, "handoff: wrote %s (%d bytes)\n", path, len(rendered))
		return nil
	}
}

func (e Env) openTerminal(spec termlaunch.Spec) (string, error) {
	if e.OpenTerminal != nil {
		return e.OpenTerminal(spec)
	}
	return termlaunch.Open(context.Background(), spec, termlaunch.DefaultEnvironment())
}

func (e Env) clipboard(data []byte) error {
	if e.Clipboard != nil {
		return e.Clipboard(data)
	}
	cmd := exec.CommandContext(context.Background(), "pbcopy")
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("pbcopy: %w: %s", err, msg)
		}
		return fmt.Errorf("pbcopy: %w", err)
	}
	return nil
}
