package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// launchSpec is everything needed to start a destination agent, in this
// terminal or in a new window.
type launchSpec struct {
	Destination handoffDestination
	// Binary is the agent's resolved executable; Args exclude it.
	Binary string
	Args   []string
	Dir    string
	// Env is the child's complete environment.
	Env []string
	// HandoffFile is the launch copy the prompt names.
	HandoffFile string
}

// handoffSessionEnv names the variables through which an agent tells the
// commands it runs about its own session: every currentSessionEnv entry and
// more. A launched agent inherits none of them, or it would take itself for
// part of the calling session (and a Codex child of Cursor would later look
// like Cursor to handoff). Settings that merely share a prefix
// (CLAUDE_CODE_USE_BEDROCK, CODEX_HOME) are the person's configuration and
// must survive, so this is an explicit list, never a prefix match.
var handoffSessionEnv = func() []string {
	names := []string{
		"CLAUDECODE",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_SESSION_ATTENDED",
		"CLAUDE_CODE_EXECPATH",
		"CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN",
		"CLAUDE_CODE_HOST_SESSION_ID",
		"CLAUDE_PID",
		"CLAUDE_EFFORT",
		"AI_AGENT",
		"CODEX_THREAD_ID",
		"CODEX_SESSION_ID",
		"CODEX_CI",
		"CODEX_SANDBOX",
		"CODEX_SANDBOX_NETWORK_DISABLED",
		"CODEX_PERMISSION_PROFILE",
		"CODEX_VERSION",
		"CURSOR_AGENT",
	}
	for _, v := range currentSessionEnv {
		if !slices.Contains(names, v.key) {
			names = append(names, v.key)
		}
	}
	return names
}()

// agentCommand is how one destination is started: the executables to try in
// order, what to install when none is found, the flag naming the directory
// it may read, and whether it takes `--` before the prompt.
type agentCommand struct {
	binaries     []string
	install      string
	dirFlag      string
	endOfOptions bool
}

// agentCommands was verified by probing each CLI. Claude Code refuses to
// read a file outside its project unless the file's directory is added, so
// it is given the handoff's directory and started in the checkout; Codex
// and Cursor take the checkout as a flag and can read the whole disk.
// Claude Code (commander) and Codex (clap) end options at `--`; Cursor's
// agent is not open source, so its prompt goes last without one.
var agentCommands = map[handoffDestination]agentCommand{
	handoffDestinationClaude: {binaries: []string{"claude"}, install: "Claude Code", dirFlag: "--add-dir", endOfOptions: true},
	handoffDestinationCodex:  {binaries: []string{"codex"}, install: "Codex", dirFlag: "--cd", endOfOptions: true},
	handoffDestinationCursor: {binaries: []string{"agent", "cursor-agent"}, install: "Cursor's CLI", dirFlag: "--workspace"},
}

// buildLaunchSpec resolves dest's executable to an absolute path and
// arranges its arguments: the directory flag, extra (configured arguments,
// then those after `--`), and the prompt last, after `--` where the agent
// accepts it so no option takes the prompt as its value (Claude's --add-dir
// takes any number of directories).
func buildLaunchSpec(dest handoffDestination, prompt, handoffFile, dir string, extra []string, env launchSpecDependencies) (launchSpec, error) {
	command, ok := agentCommands[dest]
	if !ok {
		return launchSpec{}, fmt.Errorf("unknown agent %q", dest)
	}
	if slices.Contains(extra, "--") {
		// Words after a second `--` would come before the prompt as
		// positional arguments, and the agent would take one as its prompt.
		return launchSpec{}, fmt.Errorf("arguments for %s cannot include `--`: the handoff prompt goes after it", dest)
	}
	var binary string
	for _, name := range command.binaries {
		path, err := env.lookPath(name)
		if err == nil {
			// A new window may start elsewhere, with another PATH.
			binary, err = filepath.Abs(path)
		}
		if err == nil {
			break
		}
		binary = ""
	}
	if binary == "" {
		return launchSpec{}, fmt.Errorf("could not find %s: install %s or put it on PATH", strings.Join(command.binaries, " or "), command.install)
	}
	dirArg := dir
	if dest == handoffDestinationClaude {
		// Only the launch copy's own directory, not every saved handoff.
		dirArg = filepath.Dir(handoffFile)
	}
	args := slices.Concat([]string{command.dirFlag, dirArg}, extra)
	if command.endOfOptions {
		args = append(args, "--")
	}
	args = append(args, prompt)
	return launchSpec{Destination: dest, Binary: binary, Args: args, Dir: dir, Env: childEnv(env.environ()), HandoffFile: handoffFile}, nil
}

// childEnv is environ without handoffSessionEnv.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(handoffSessionEnv, name) {
			out = append(out, kv)
		}
	}
	return out
}
