package cli

import (
	"fmt"
	"io"
	"strings"
)

var commandHelp = map[string]string{
	"setup": `Usage: agent-archive setup

Choose apps and projects, connect storage, then review and enable capture.
Run again to continue saved setup or edit capture, storage, or retention.
Credentials are entered privately; never pass them as command arguments.
Example: agent-archive setup
`,
	"status": `Usage: agent-archive status [--json]

Show local capture evidence, background health, and a next step.
No conversations are printed and no cloud request is made.
--json prints the same status as a versioned JSON document.
Example: agent-archive status --json
`,
	"sync": `Usage: agent-archive sync

Collect and upload once. Failures leave pending work available to retry.
If paused, no work is started; run agent-archive resume first.
Example: agent-archive sync
`,
	"pause": `Usage: agent-archive pause

Persistently pause collection, uploads, and cleanup. Current work must finish
before the command can confirm pause; retry if another operation is running.
Already registered sessions can catch up after resume, including activity
written during the pause. New sessions begun while paused are not imported.
Example: agent-archive pause
`,
	"resume": `Usage: agent-archive resume

Resume scheduled collection, uploads, and cleanup. Already registered sessions
can catch up, including activity written during the pause. For an immediate
pass, run agent-archive sync.
Example: agent-archive resume
`,
	"uninstall": `Usage: agent-archive uninstall [--delete-local-data]

Remove hooks and the background collector. Keep local evidence, settings,
and credentials by default, so setup can restore the installation.
--delete-local-data also removes owned local files and stored credentials,
including unpublished evidence, after a separate confirmation.
Remote archives and unrelated files are always kept.
Example: agent-archive uninstall
`,
	"list": `Usage: agent-archive list [options]

Find sessions using metadata; does not download conversation content.
  --harness codex|claude|cursor   Filter by application
  --model NAME                   Filter by model
  --skill NAME                   Filter by skill
  --skill-sha256 HEX             Filter by exact lowercase skill SHA-256
  --skill-usage used|available|eligible_no_use
                                 eligible_no_use cannot return sessions yet:
                                 no parser version records both a complete
                                 eligible-skill set and complete use
                                 observation, so non-use is never proven. The
                                 value stays accepted for forward compatibility.
  --since DATE|AGE               For example 2026-01-31 or 7d
  --complete                     Require complete parser coverage
  --no-cache                     Download every metadata sidecar instead of
                                 reusing unchanged ones from the local
                                 metadata cache (metadata only; never
                                 conversation content)
Example: agent-archive list --skill review-pr --skill-sha256 HASH --since 7d
`,
	"show": `Usage: agent-archive show ID [--harness NAME] [--normalized]

Print session metadata as JSON. --normalized explicitly downloads and verifies
its source bundle and prints conversation content as well.
Example: agent-archive show SESSION_ID --normalized
`,
	"handoff": `Usage: agent-archive handoff ID|--latest|--file PATH [options]

Print a session as a prompt another coding agent can continue from. This
prints conversation content, filtered as it is for the archive: injected
instructions and credentials removed, tool output trimmed, edit bodies left
out. A session on this machine is read from its transcript now, without
waiting for a sync; otherwise it is downloaded from the archive.
  --latest              The most recent session for the project
  --project DIR         Project for --latest (default: current directory)
  --harness NAME        claude, codex, or cursor
  --file PATH           Render a native transcript directly (needs --harness);
                        works for sessions the archive never captured
  --source auto|local|archive
  --max-bytes N         Output limit, default 120000 (about 30k tokens); 0 for
                        no limit. When trimmed, the full version is saved in
                        the data directory for 7 days and its path is named
                        at the end
  --format markdown|json
  --output FILE         Write to FILE (mode 0600); --force replaces it
  --no-preamble         Omit the note addressed to the receiving agent
Example: claude "$(agent-archive handoff --latest --harness codex)"
Example: codex "$(agent-archive handoff --latest --harness claude)"
`,
	"backfill": `Usage: agent-archive backfill --dry-run [options]

Plan importing the Claude Code, Codex, and Cursor sessions already on this Mac
that the archive has not captured. Shows each project with its session count
per app, and why any session would not be imported. Prints project folders
and counts only, never conversation content. Nothing is written or uploaded.
  --harness NAME        Only claude, codex, or cursor (repeatable)
  --project DIR         Only this project; it need not still exist (repeatable)
  --since DATE          Sessions started on or after DATE (YYYY-MM-DD, local)
  --until DATE          Sessions started on or before DATE
  --include-home        Include sessions run from the home folder
  --include-temp        Include sessions run from temporary directories
  --include-removed     Include sessions retention or undo removed
  --dry-run             Print the plan and exit
  --json                With --dry-run, print the plan as JSON
Importing is not available yet; this version prints the plan only.
Example: agent-archive backfill --dry-run --since 2026-09-01
`,
	"feedback": `Usage: agent-archive feedback ID --file PATH

Attach an explicit user assessment to a locally captured session. The file is
read locally, privacy-filtered, and queued for the next collection pass. Its
path is not archived. Collection remains paused until you resume it.
Example: agent-archive feedback SESSION_ID --file /private/path/feedback.txt
`,
}

// Preflight never resolves paths, credentials, or runtime dependencies.
func commandPreflight(args []string, out, errOut io.Writer) (bool, int) {
	cmd := args[0]
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		if len(args) == 1 {
			fmt.Fprint(out, usage)
			return true, 0
		}
		if cmd == "help" && len(args) == 2 {
			if help, ok := commandHelp[args[1]]; ok {
				fmt.Fprint(out, help)
				return true, 0
			}
		}
		fmt.Fprintln(errOut, "Use agent-archive help COMMAND.")
		return true, 2
	}
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		if len(args) != 1 {
			fmt.Fprintln(errOut, "--version takes no arguments.")
			return true, 2
		}
		return false, 0
	}
	help, public := commandHelp[cmd]
	if !public {
		return false, 0
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			fmt.Fprint(out, help)
			return true, 0
		}
	}
	if cmd == "list" || cmd == "show" || cmd == "feedback" || cmd == "handoff" || cmd == "backfill" {
		return false, 0
	} // Their parsers validate before I/O.
	allowed := ""
	if cmd == "status" {
		allowed = "--json"
	}
	if cmd == "uninstall" {
		allowed = "--delete-local-data"
	}
	if len(args) > 1 && (len(args) != 2 || allowed == "" || args[1] != allowed) {
		fmt.Fprintf(errOut, "agent-archive %s: unexpected arguments %s\nRun agent-archive %s --help.\n", cmd, strings.Join(args[1:], " "), cmd)
		return true, 2
	}
	return false, 0
}
