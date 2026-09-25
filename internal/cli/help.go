package cli

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

var commandHelp = map[string]string{
	"setup": `Usage: agent-archive setup [--abandon-recovery]

Choose apps and projects, connect storage, then review and enable capture.
Run again to continue saved setup or edit capture, storage, or retention.
Credentials are entered privately; never pass them as command arguments.
Setup asks questions, so it needs a terminal.
An interrupted setup is recovered on the next run. If recovery stops because
a file it changed was edited since, --abandon-recovery keeps every file as it
is now and discards the interrupted setup; then run setup again.
Example: agent-archive setup
`,
	"status": `Usage: agent-archive status [--json]

Show local capture evidence, background health, and a next step.
No conversations are printed and no cloud request is made.
--json prints the same status as a versioned JSON document. In it,
storage_verified_at is when setup's storage check last passed, and
storage_access_confirmed_at is the latest confirmation of access (by
"setup" or the "collector", in storage_access_confirmed_by).
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
	"uninstall": `Usage: agent-archive uninstall [--delete-local-data] [--yes]

Remove hooks and the background collector. Keep local evidence, settings,
and credentials by default, so setup can restore the installation.
--delete-local-data also removes owned local files and stored credentials,
including unpublished evidence, after a separate confirmation.
--yes skips the confirmations; it is required without a terminal.
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
                                 How --skill matches (default used).
                                 eligible_no_use cannot return sessions yet:
                                 no parser version records both a complete
                                 eligible-skill set and complete use
                                 observation, so non-use is never proven. The
                                 value stays accepted for forward compatibility.
  --since DATE|TIME|AGE          Captured at or after a date (2026-01-31,
                                 from midnight UTC; backfill's --since uses
                                 your local day), an RFC 3339 time, or an
                                 age (7d, 12h)
  --complete                     Require complete parser coverage and no
                                 capture gaps
  --imported                     Only sessions agent-archive backfill imported
  --hook-captured                Only sessions hooks captured as they ran
  --no-cache                     Download every metadata sidecar instead of
                                 reusing unchanged ones from the local
                                 metadata cache (metadata only; never
                                 conversation content)
  --json                         Print {"schema_version": 1, "sessions": [...]}:
                                 each matching session's metadata, as show
                                 prints it (never conversation content). A
                                 query that cannot return sessions yet has
                                 "sessions": [] and an "unavailable" reason.
Example: agent-archive list --skill review-pr --skill-sha256 HASH --since 7d
`,
	"show": `Usage: agent-archive show SESSION_ID [--harness NAME] [--normalized] [--json]

Print session metadata as JSON (--json is accepted, as for list and status).
An imported session also shows origin, imported_at, and started_at_source.
  --harness NAME        The session's app, if the same SESSION_ID exists under
                        more than one
  --normalized          Also download and verify the source bundle, and print
                        conversation content as well
  --json                JSON, the default and only format
Example: agent-archive show SESSION_ID --normalized
`,
	"handoff": `Usage: agent-archive handoff SESSION_ID|--latest|--file PATH [options]

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
                        Where the content comes from: auto (default) reads
                        this Mac's transcript when there is one, else the
                        archive; local or archive uses only that one
  --max-bytes N         Output limit, default 120000 (about 30k tokens); 0 for
                        no limit. When trimmed, the full version is saved in
                        the data directory for 7 days and its path is named
                        at the end
  --format markdown|json
                        markdown (default) prints the prompt; json prints
                        the structured handoff document it is rendered from
  --output FILE         Write to FILE (mode 0600) instead of printing it
  --force               With --output, replace FILE if it exists
  --no-preamble         Omit the note addressed to the receiving agent
Example: claude "$(agent-archive handoff --latest --harness codex)"
Example: codex "$(agent-archive handoff --latest --harness claude)"
`,
	"backfill": `Usage: agent-archive backfill [options]
       agent-archive backfill history
       agent-archive backfill undo [IMPORT_ID] [--project DIR] [--yes]
                                [--restore-retention]

Import the Claude Code, Codex, and Cursor sessions already on this Mac that
the archive has not captured. First shows each project with its session count
per app, and why any session is not imported; nothing is written until you
confirm. Projects the import needs are added to capture. Prints project
folders and counts only, never conversation content.
  --harness NAME        Only claude, codex, or cursor (repeatable)
  --project DIR         Only this project; it need not still exist (repeatable)
  --since DATE|TIME|AGE Sessions started on or after this local day (list's
                        --since uses UTC): a date (2026-09-01), an RFC 3339
                        time, or an age (7d, 12h); a time or age selects
                        from the start of its local day
  --until DATE|TIME|AGE Sessions started on or before this local day
  --include-home        Include sessions run from the home folder
  --include-temp        Include sessions run from temporary directories
  --include-removed     Include sessions retention or undo removed
  --dry-run             Print the plan and exit; nothing is written
  --json                With --dry-run, print the plan as JSON
  --yes                 Skip the confirmation (required without a terminal)
  --background          Register the sessions and exit; the background
                        collector uploads them
See agent-archive help backfill history and agent-archive help backfill undo.
Example: agent-archive backfill --dry-run --since 30d
`,
	"backfill history": `Usage: agent-archive backfill history

List past imports, oldest first: each import's IMPORT_ID, when it started,
how many sessions and projects it added, and its upload state (waiting,
uploaded, interrupted, or undone). Reads this Mac's records only.
Example: agent-archive backfill history
`,
	"backfill undo": `Usage: agent-archive backfill undo [IMPORT_ID] [--project DIR] [--yes]
                                [--restore-retention]

Remove the latest import, or the import IMPORT_ID from backfill history: its
sessions are deleted from the bucket and this Mac, and the projects it added
are excluded from capture. Shows what it will do and asks first.
Hook-captured sessions and the apps' own files are never touched. If the
import raised retention, undo offers to put the shorter retention back,
saying how many sessions (from anywhere, not only the import) that deletes.
  --project DIR         Only this project's sessions from the import;
                        retention is left alone
  --yes                 Skip the confirmation (required without a terminal);
                        retention is left as it is
  --restore-retention   With --yes, also put back the retention from before
                        the import, deleting sessions older than it
Example: agent-archive backfill undo --project ~/src/old-experiment
`,
	"version": `Usage: agent-archive version

Print the installed version (also: agent-archive --version, -v).
Example: agent-archive --version
`,
	"feedback": `Usage: agent-archive feedback SESSION_ID --file PATH

Attach an explicit user assessment to a locally captured session. The file is
read locally, privacy-filtered, and queued for the next collection pass. Its
path is not archived. While capture is paused, it waits until you resume.
Example: agent-archive feedback SESSION_ID --file /private/path/feedback.txt
`,
}

// Preflight never resolves paths, credentials, or runtime dependencies.
func commandPreflight(args []string, out, errOut io.Writer) (bool, int) {
	cmd := args[0]
	//lint:ignore LV1001 cmd is raw argv; these are the spellings that ask for help
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		if len(args) == 1 {
			terminal.Print(out, usage)
			return true, 0
		}
		if cmd == "help" && (len(args) == 2 || len(args) == 3) {
			if help, ok := commandHelp[strings.Join(args[1:], " ")]; ok {
				terminal.Print(out, help)
				return true, 0
			}
		}
		terminal.Println(errOut, "Use agent-archive help COMMAND.")
		return true, 2
	}
	//lint:ignore LV1001 cmd is raw argv; these are the spellings that ask for the version
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		if len(args) == 2 && isHelpFlag(args[1]) {
			terminal.Print(out, commandHelp["version"])
			return true, 0
		}
		if len(args) != 1 {
			terminal.Printf(errOut, "agent-archive: version: unexpected argument %q; run agent-archive version --help\n", args[1])
			return true, 2
		}
		return false, 0
	}
	help, public := commandHelp[cmd]
	if !public {
		return false, 0
	}
	// A subcommand (backfill undo, backfill history) has help of its own.
	if len(args) > 1 {
		if sub, ok := commandHelp[cmd+" "+args[1]]; ok {
			help = sub
		}
	}
	if slices.ContainsFunc(args[1:], isHelpFlag) {
		terminal.Print(out, help)
		return true, 0
	}
	// Each command's own flag set is the one source of truth for its
	// arguments, and it validates them before any I/O.
	return false, 0
}

// isHelpFlag reports whether a raw argument asks for help.
func isHelpFlag(arg string) bool {
	//lint:ignore LV1001 arg is raw argv, checked for a help flag
	return arg == "--help" || arg == "-h" || arg == "-help"
}

// onNewCommandFlags, when set (only by tests), sees every command flag set
// as it is made, so a test can check each flag against the help text.
var onNewCommandFlags func(*commandFlags)

// commandFlags is a public command's flag set. Every command reports a bad
// command line the same way, through usageError: one line on stderr naming
// the problem and the command's help, and exit 2. The flag package's own
// messages and usage dump (with single-dash flag names) never reach the
// user.
type commandFlags struct {
	*flag.FlagSet
	errOut io.Writer
}

// newCommandFlags returns the flag set of command, named as the user types
// it ("backfill undo").
func newCommandFlags(command string, errOut io.Writer) *commandFlags {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	flags := &commandFlags{FlagSet: fs, errOut: errOut}
	if onNewCommandFlags != nil {
		onNewCommandFlags(flags)
	}
	return flags
}

// usageError reports a problem with the command line and returns exit
// code 2.
func (f *commandFlags) usageError(format string, args ...any) int {
	terminal.Printf(f.errOut, "agent-archive: %s: %s; run agent-archive %s --help\n", f.Name(), fmt.Sprintf(format, args...), f.Name())
	return 2
}

// parse parses the flags in args, leaving positional arguments in Args. It
// reports a bad flag and returns false; the command then exits 2.
func (f *commandFlags) parse(args []string) bool {
	if err := f.Parse(args); err != nil {
		f.usageError("%s", describeFlagError(err))
		return false
	}
	return true
}

// parseFlagsOnly is parse for a command that takes no positional
// arguments: one is reported like a bad flag.
func (f *commandFlags) parseFlagsOnly(args []string) bool {
	if !f.parse(args) {
		return false
	}
	if f.NArg() != 0 {
		f.usageError("unexpected argument %q", f.Arg(0))
		return false
	}
	return true
}

// parseWithArgument is parse for a command that takes at most one
// positional argument (a session ID), before or after its flags: the flag
// package otherwise stops at the first positional value. It returns the
// argument, or "" when there is none.
func (f *commandFlags) parseWithArgument(args []string) (string, bool) {
	if !f.parse(args) {
		return "", false
	}
	if f.NArg() == 0 {
		return "", true
	}
	argument := f.Arg(0)
	if !f.parse(f.Args()[1:]) {
		return "", false
	}
	if f.NArg() != 0 {
		f.usageError("unexpected argument %q", f.Arg(0))
		return "", false
	}
	return argument, true
}

var flagValueError = regexp.MustCompile(`^invalid (?:boolean )?value ("(?:[^"\\]|\\.)*") for (?:flag )?-+([^:]+): (.*)$`)

// describeFlagError rewrites the flag package's error in the CLI's terms,
// with flags spelled the way help shows them (--name).
func describeFlagError(err error) string {
	message := err.Error()
	flagName := func(s string) string { return "--" + strings.TrimLeft(strings.TrimSpace(s), "-") }
	switch {
	case strings.HasPrefix(message, "flag provided but not defined: "):
		return "unknown flag " + flagName(strings.TrimPrefix(message, "flag provided but not defined: "))
	case strings.HasPrefix(message, "flag needs an argument: "):
		return flagName(strings.TrimPrefix(message, "flag needs an argument: ")) + " needs a value"
	case strings.HasPrefix(message, "bad flag syntax: "):
		return "bad flag " + strings.TrimPrefix(message, "bad flag syntax: ")
	}
	if m := flagValueError.FindStringSubmatch(message); m != nil {
		return fmt.Sprintf("invalid value %s for --%s: %s", m[1], m[2], m[3])
	}
	return message
}
