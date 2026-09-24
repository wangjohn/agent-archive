package cli

import (
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runSyncCommand implements `agent-archive sync`: one explicit collection
// pass, preserving queued work on failure. It respects paused state and
// does not implicitly resume, per the spec.
func runSyncCommand(_ []string, stdout, stderr io.Writer, env Env) int {
	result, err := runOnePass(env, false)
	if err != nil {
		switch {
		case errors.Is(err, errPaused):
			terminal.Println(stdout, "agent-archive: "+err.Error())
			return 0
		case errors.Is(err, errNotSetUp):
			terminal.Println(stderr, "agent-archive: "+err.Error())
		case errors.Is(err, local.ErrBusy):
			terminal.Println(stderr, "agent-archive: sync: another sync is already running")
		default:
			terminal.Printf(stderr, "agent-archive: sync: %v\n", err)
			// A Keychain failure has one specific fix; say which.
			if action := credentials.RecoveryAction(err); action != "" {
				terminal.Println(stderr, "agent-archive: sync: "+action)
			}
		}
		return 1
	}
	terminal.Printf(stdout, "Scanned %d session(s): %d published, %d unchanged, %d failed.\n",
		result.Scanned, len(result.Published), len(result.Skipped), len(result.Errors))
	for id, sessionErr := range result.Errors {
		terminal.Printf(stdout, "  %s: %v\n", id, sessionErr)
	}
	if len(result.Errors) > 0 {
		return 1
	}
	return 0
}
