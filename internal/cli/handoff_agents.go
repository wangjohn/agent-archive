package cli

import (
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
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
		cursorAgentEnv,
	}
	for _, v := range currentSessionEnv {
		if !slices.Contains(names, v.key) {
			names = append(names, v.key)
		}
	}
	return names
}()

// buildLaunchSpec resolves dest's executable to an absolute path and
// arranges its arguments: the directory flag, extra (configured arguments,
// then those after `--`), and the prompt last, after `--` where the agent
// accepts it so no option takes the prompt as its value (Claude's --add-dir
// takes any number of directories).
func buildLaunchSpec(dest handoffDestination, prompt, handoffFile, dir string, extra []string, env launchSpecDependencies) (launchSpec, error) {
	integration, ok := registryFor(env).Lookup(string(dest))
	if !ok || integration.Launcher == nil {
		return launchSpec{}, fmt.Errorf("unknown agent %q", dest)
	}
	command := integration.Launcher.Executables()
	args, err := integration.Launcher.Args(agentapi.LaunchRequest{ProjectDir: dir, Prompt: prompt, HandoffPath: handoffFile, ExtraArgs: extra})
	if err != nil {
		return launchSpec{}, err
	}
	var binary string
	for _, name := range command.Names {
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
		return launchSpec{}, fmt.Errorf("could not find %s: install %s or put it on PATH", strings.Join(command.Names, " or "), command.Install)
	}
	dest = handoffDestination(integration.Descriptor.ID)
	return launchSpec{Destination: dest, Binary: binary, Args: args, Dir: dir, Env: childEnv(env.environ()), HandoffFile: handoffFile}, nil
}

// childEnv is environ without handoffSessionEnv, and without envTrace: the
// trace was of the handoff, not of the agent's own agent-archive commands.
func childEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(handoffSessionEnv, name) && name != envTrace {
			out = append(out, kv)
		}
	}
	return out
}
