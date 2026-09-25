package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runHookCommand implements the hidden `_hook` entry point hooks.Merge
// installs into each harness's own hook configuration. A hook has a short
// timeout (2s, per hooks.Merge) and must never block the user's turn, so
// this always exits 0; a problem is reported to stderr only, matching the
// spec's failure table ("Hook cannot write a request: task continues,
// diagnostic is available outside model context"). That includes a panic:
// Go exits 2 on one, which Claude Code treats as a blocking error (it
// erases the prompt on UserPromptSubmit and feeds the stack trace to the
// model on Stop), so a panic is recovered, recorded, and exits 0 too.
func runHookCommand(args []string, stdin io.Reader, stderr io.Writer, env Env) (code int) {
	var (
		home    string
		harness = new(string)
		payload map[string]any
	)
	defer func() {
		if r := recover(); r != nil {
			terminal.Printf(stderr, "agent-archive: hook: internal error: %v\n", r)
			capture.RecordFailure(home, *harness, payload)
			code = 0
		}
	}()
	fs := flag.NewFlagSet("_hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness = fs.String("harness", "", "harness name (codex, claude, cursor)")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	// A hook that sends no or malformed JSON is treated as a no-op, not an
	// error: some hook events (per the harness's own docs) carry no useful
	// fields at all, and we must never fail loudly on the harness's input.
	_ = json.NewDecoder(stdin).Decode(&payload)

	// Resolved without creating it: a hook left behind after the data
	// directory was deleted has nothing to record and must not recreate it.
	home, err := env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: hook: resolve home: %v\n", err)
		return 0
	}
	if _, err := os.Stat(home); errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err := capture.HandleEvent(home, *harness, payload, env.now()); err != nil {
		terminal.Printf(stderr, "agent-archive: hook: %v\n", err)
	}
	return 0
}
