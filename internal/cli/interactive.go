package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

// envNonInteractive is the switch that keeps agent-archive from asking
// anything: no session picker, no confirmation prompt, no pager, no alternate
// screen. See docs/reference/configuration.md.
const envNonInteractive = "AGENT_ARCHIVE_NONINTERACTIVE"

// cursorAgentEnv is set by Cursor's agent for the commands it runs. Cursor
// exposes no session ID, so it is evidence that an agent is running the
// command, and handoff --to falls back to the newest Cursor session for the
// working directory.
const cursorAgentEnv = "CURSOR_AGENT"

// agentShellEnv lists the variables whose presence means a coding agent's
// shell is running this command: the ones handoff reads to find the calling
// session, plus Cursor's.
func agentShellEnv() []string {
	keys := make([]string, 0, len(currentSessionEnv)+1)
	for _, v := range currentSessionEnv {
		keys = append(keys, v.key)
	}
	return append(keys, cursorAgentEnv)
}

// nonInteractiveMode says whether interaction is off and why.
type nonInteractiveMode struct {
	on bool
	// reason completes "Prompts are off because ..." for a person who did
	// not ask for that.
	reason string
}

// parseSwitch reads an on/off setting: 1, true, yes, or on and 0, false, no,
// or off, in any case.
func parseSwitch(value string) (on, valid bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// nonInteractive decides whether this run must not ask anything. An explicit
// AGENT_ARCHIVE_NONINTERACTIVE wins, either way. Unset (or empty) means auto:
// on when an agent variable is set. A value that is not a recognized
// spelling is an error for the caller to report, and until then counts as on,
// so a typo can never turn a prompt back on in an agent's shell.
func (e Env) nonInteractive() (nonInteractiveMode, error) {
	if value, set := e.lookupEnv(envNonInteractive); set && strings.TrimSpace(value) != "" {
		on, valid := parseSwitch(value)
		if !valid {
			return nonInteractiveMode{on: true, reason: fmt.Sprintf("%s=%q is not a valid setting", envNonInteractive, value)},
				fmt.Errorf("%s=%q is not a valid setting; use 1 or 0, or unset it for automatic", envNonInteractive, value)
		}
		if !on {
			return nonInteractiveMode{}, nil
		}
		return nonInteractiveMode{on: true, reason: envNonInteractive + " is set"}, nil
	}
	for _, key := range agentShellEnv() {
		if value, set := e.lookupEnv(key); set && strings.TrimSpace(value) != "" {
			return nonInteractiveMode{on: true, reason: key + " is set, which means an agent is running this command"}, nil
		}
	}
	return nonInteractiveMode{}, nil
}

// interactive reports whether this command may ask the person something by
// way of stream (a picker, a confirmation, a pager, the alternate screen):
// stream is a terminal, and interaction is not switched off. Everything that
// decides to interact goes through it, never through isTerminal, so the
// switch cannot be bypassed; TestTerminalChecksAreClassified enforces that.
func (e Env) interactive(stream any) bool {
	mode, _ := e.nonInteractive()
	return !mode.on && e.isTerminal(stream)
}

// blockedByNonInteractive reports whether stream is a terminal that would
// have been interactive but for the switch, and if so what turned it on.
func (e Env) blockedByNonInteractive(stream any) (reason string, blocked bool) {
	mode, _ := e.nonInteractive()
	if !mode.on || !e.isTerminal(stream) {
		return "", false
	}
	return mode.reason, true
}

// overrideHint is the sentence a refusal ends with when the switch, not the
// absence of a terminal, is why it had to refuse: how a person overrides it.
// It is empty otherwise, so a piped run does not hear about a switch that
// would not have helped.
func (e Env) overrideHint(stream any) string {
	reason, blocked := e.blockedByNonInteractive(stream)
	if !blocked {
		return ""
	}
	return fmt.Sprintf(" Prompts are off because %s; to be asked here anyway, run with %s=0.", reason, envNonInteractive)
}

// nonInteractiveSettingUsable reports a bad AGENT_ARCHIVE_NONINTERACTIVE
// once, as a usage error, before the command named by args runs. The hidden
// hook and collector commands never ask anything and must stay silent, and
// help and version stay usable so the setting can be looked up.
func nonInteractiveSettingUsable(args []string, stderr io.Writer, env Env) bool {
	if len(args) > 0 {
		switch args[0] {
		case "_hook", "_collect", "help", "-h", "--help", "version", "-v", "--version":
			return true
		}
	}
	if _, err := env.nonInteractive(); err != nil {
		terminal.Printf(stderr, "agent-archive: %v\n", err)
		return false
	}
	return true
}
