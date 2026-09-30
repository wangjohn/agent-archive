package cli

import (
	"io"

	"github.com/wangjohn/agent-archive/internal/trace"
)

// envTrace names the switch that prints where a command's time went.
const envTrace = "AGENT_ARCHIVE_TRACE"

// tracedCommands are the names a trace's root span may carry.
var tracedCommands = map[string]bool{
	"status": true, "sync": true, "pause": true, "resume": true, "setup": true, "uninstall": true,
	"list": true, "show": true, "stats": true, "feedback": true, "handoff": true, "backfill": true, "purge": true,
}

// startTrace begins recording when AGENT_ARCHIVE_TRACE is on (1, true, yes,
// or on) and returns the function that ends the command's span and writes
// the timing tree to stderr. It is a diagnostic, so a value that is not a
// switch leaves it off rather than failing the command. The internal
// commands the hooks and the collector run are not listed and never trace:
// their stderr is not a person's.
func startTrace(command string, stderr io.Writer, env Env) (finish func()) {
	value, _ := env.lookupEnv(envTrace)
	// Only the commands listed: the root span is named for the command,
	// and help, version or an unknown word has nothing worth timing.
	if on, _ := parseSwitch(value); !on || !tracedCommands[command] {
		return func() {}
	}
	disable := trace.Enable()
	root := trace.Start(command)
	return func() {
		root.End()
		trace.Write(stderr)
		disable()
	}
}
