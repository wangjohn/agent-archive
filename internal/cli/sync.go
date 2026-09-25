package cli

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// waitingSummary is sync's count of publications held for the upload
// interval, with the local time the earliest is due, or "" when none wait.
func waitingSummary(waiting []string, next time.Time) string {
	if len(waiting) == 0 {
		return ""
	}
	if next.IsZero() {
		return fmt.Sprintf("%d waiting for the upload interval, ", len(waiting))
	}
	return fmt.Sprintf("%d waiting for the upload interval (next at %s), ", len(waiting), next.Local().Format("15:04"))
}

// runSyncCommand implements `agent-archive sync`: one explicit collection
// pass, preserving queued work on failure. It respects paused state and
// does not implicitly resume, per the spec.
func runSyncCommand(args []string, stdout, stderr io.Writer, env Env) int {
	if !newCommandFlags("sync", stderr).parseFlagsOnly(args) {
		return 2
	}
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
	terminal.Printf(stdout, "Scanned %d session(s): %d published, %s%d unchanged, %d failed.\n",
		result.Scanned, len(result.Published), waitingSummary(result.Waiting, result.NextReadyAt), len(result.Skipped), len(result.Errors))
	for id, sessionErr := range result.Errors {
		terminal.Printf(stdout, "  %s: %v\n", id, sessionErr)
	}
	if len(result.Errors) > 0 {
		return 1
	}
	return 0
}
