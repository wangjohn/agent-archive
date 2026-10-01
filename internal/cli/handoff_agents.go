package cli

import (
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
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

// buildLaunchSpec resolves dest's executable to an absolute path and
// arranges its arguments: the directory flag, extra (configured arguments,
// then those after `--`), and the prompt last, after `--` where the agent
// accepts it so no option takes the prompt as its value (Claude's --add-dir
// takes any number of directories).
func buildLaunchSpec(dest handoffDestination, prompt, handoffFile, dir string, extra []string, env launchSpecDependencies) (launchSpec, error) {
	launcher, ok := env.launcherLookup().Launcher(string(dest))
	if !ok {
		return launchSpec{}, fmt.Errorf("unknown agent %q", dest)
	}
	command := launcher.Executables()
	args, err := launcher.Args(agentapi.LaunchRequest{ProjectDir: dir, Prompt: prompt, HandoffPath: handoffFile, ExtraArgs: extra})
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
	dest = handoffDestination(agentmeta.Canonical(catalogFor(env), string(dest)))
	return launchSpec{Destination: dest, Binary: binary, Args: args, Dir: dir, Env: childEnv(env.environ(), launchEnvironmentKeys(env)), HandoffFile: handoffFile}, nil
}

// childEnv removes the composed native session keys and trace state.
func childEnv(environ, unset []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(unset, name) {
			out = append(out, kv)
		}
	}
	return out
}
