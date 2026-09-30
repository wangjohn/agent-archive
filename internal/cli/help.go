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
	"purge": `Usage: agent-archive purge plan [--mode unreferenced|old-filter]
       [--before-filter VERSION] [--no-pager]
       agent-archive purge apply PLAN [--yes]

Create a private five-minute deletion plan, then review its exact keys.
Plan lists still-current older-filter sessions separately and never proposes
their current sources for deletion. Pause every Mac uploading to this prefix
before apply. A versioned bucket keeps noncurrent versions and delete markers.
`,
	"purge plan": `Usage: agent-archive purge plan [--mode unreferenced|old-filter]
       [--before-filter VERSION] [--no-pager]

Read metadata and list source objects without deleting anything. Old-filter
mode selects only unreferenced sources whose filter version is below VERSION;
both modes report still-current older-filter sessions separately. On a
terminal, the plan is paged through $PAGER unless --no-pager.
`,
	"purge apply": `Usage: agent-archive purge apply PLAN [--yes]

Pause every uploading Mac first. Confirm the plan digest or use --yes.
Apply rechecks remote metadata before each deletion and writes a resumable
report next to the plan. A plan expires five minutes after creation.
`,
	"setup": `Usage: agent-archive setup [--abandon-recovery] [--verbose]
       agent-archive setup --yes [--provider r2|s3 ...] [--project DIR ...]
               [--skill-evidence none|metadata|body]

Choose apps and projects, connect storage, then review and enable capture.
Run again to continue saved setup or edit capture, storage, or retention.
Credentials are entered privately; never pass them as command arguments.
Setup asks questions, so it needs a terminal, unless --yes is given.
An interrupted setup is recovered on the next run.
  --abandon-recovery    If recovery stops because a file it changed was
                        edited since, keep every file as it is now and
                        discard the interrupted setup; then run setup again
  --verbose             If the storage check fails, also show the storage
                        provider's own error under the diagnosis
  --yes                 Ask nothing: take the answers below, the saved
                        settings, and the apps found; run the same storage
                        check; and save. Refuses if an answer is missing
  --provider r2|s3      Storage provider (default: the saved storage)
  --bucket NAME         Bucket (R2: or from --r2-account's URL)
  --r2-account ACCOUNT|URL
                        The R2 account, or the bucket URL the dashboard shows
  --r2-access-key-id KEY
                        The R2 access key (or AGENT_ARCHIVE_R2_ACCESS_KEY_ID;
                        default: keep the saved key)
  --aws-profile NAME    S3: the AWS profile with access to the bucket
  --region REGION       S3: the bucket's region (default: the profile's)
  --project DIR         Capture this project, besides any saved (repeatable)
  --apps LIST           Apps to capture: codex,claude,cursor (default: the
                        saved apps, else those found on this Mac). It must
                        name every app set up now: --yes never removes one
  --skill-evidence MODE none: no filesystem skill evidence; metadata: names
                        and filtered hashes; body: filtered SKILL.md text.
                        Fresh setup defaults to metadata; earlier configs
                        without this field keep body until changed.
With --yes, the R2 secret access key is read from
AGENT_ARCHIVE_R2_SECRET_ACCESS_KEY, or else from standard input.
Example: agent-archive setup
Example: printf '%s\n' "$SECRET" | agent-archive setup --yes --provider r2 \
  --r2-account ACCOUNT_ID --bucket BUCKET --r2-access-key-id KEY_ID \
  --project ~/src/app --apps codex,claude
`,
	"status": `Usage: agent-archive status [APP] [--verbose] [--json] [--no-pager]

Show local capture evidence, background health, and a next step.
No conversations are printed and no cloud request is made.
Each app's line counts its sessions, imports and uploads, with at most
five uploading sessions under it; the screen doesn't grow with projects.
APP (claude, codex or cursor) shows that app in full: every uploading
session and a table of its projects.
--verbose adds each project's progress, skill evidence, imports and
every error, then a Details section with the codes, exact times, full
paths and raw errors behind each line.
On a terminal, status is paged through $PAGER unless --no-pager.
--json prints the same status as a versioned JSON document. In it,
storage_verified_at is when setup's storage check last passed, and
storage_access_confirmed_at is the latest confirmation of access (by
"setup" or the "collector", in storage_access_confirmed_by).
Example: agent-archive status claude
Example: agent-archive status --verbose
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
Default text columns: TITLE (first filtered prompt preview, or a short
SESSION_ID prefix when none), relative capture time, harness, project,
and a short SESSION_ID. On a terminal with an interactive stdin, list a
numbered table and pick a session to show its summary, then t for its
transcript, Enter or b to go back, or q to quit. Keys act as pressed; the
wheel, arrows, and PgUp/PgDn scroll. Piped or --json output is
never interactive, nor is any run with AGENT_ARCHIVE_NONINTERACTIVE on, as it
is inside coding agents (see the configuration reference). On a terminal
without interactive stdin, text is paged through $PAGER unless --no-pager.
  --harness codex|claude|cursor   Filter by application
  --model NAME                   Filter by model
  --skill NAME                   Filter by skill
  --skill-sha256 HEX             Filter by exact lowercase skill SHA-256
  --skill-usage used|available
                                 How --skill matches (default used).
                                 Non-use queries are unsupported because no
                                 parser proves complete eligibility and use.
  --since DATE|TIME|AGE          Captured at or after a date (2026-01-31,
                                 from midnight UTC; backfill's --since uses
                                 your local day), an RFC 3339 time, or an
                                 age (7d, 12h)
  --complete                     Require complete parser coverage and no
                                 capture gaps
  --imported                     Only sessions agent-archive backfill imported
  --hook-captured                Only sessions hooks captured as they ran
  --limit N                      Show at most N sessions, newest first
                                 (default 50; 0 for all)
  --rebuild-index                Rebuild the listing index from live metadata;
                                 scans the full archive and writes index keys
  --verbose                      Full SESSION_IDs, absolute times, origin,
                                 parser status, all models/skills, and title
  --no-pager                     Print directly; do not page through $PAGER
  --no-cache                     Bypass the local metadata cache during full
                                 scans; indexed listing always verifies live
                                 sidecars (never conversation content)
  --json                         Print {"schema_version": 4, "sessions": [...],
                                 "limit", "returned", "total_matched_known"}:
                                 "total_matched" is present only when exact;
                                 each session is live metadata. Usage errors
                                 print no JSON. Never paged or interactive.
Example: agent-archive list --skill review-pr --skill-sha256 HASH --since 7d
`,
	"show": `Usage: agent-archive show [SESSION_ID|TITLE] [options]

Print a readable summary of a session's metadata: title, when, app, models,
activity counts, skills, subagents, and capture gaps. --json prints the
metadata sidecar instead. A TITLE substring or short SESSION_ID also matches;
several matches on a terminal open a picker. With no SESSION_ID on a
terminal, browse sessions as list does: pick one for its summary, then t for
its transcript, Enter or b to go back, or q to quit. On a terminal, the
summary and transcript are paged; in the default less, scroll with the mouse
wheel, arrows, or space, search with /, and quit with q. Nothing is asked
when AGENT_ARCHIVE_NONINTERACTIVE is on, as it is inside coding agents: give
a SESSION_ID.
  --harness NAME        The session's app, if the same SESSION_ID exists under
                        more than one
  --transcript          Download and verify the source bundle, and print the
                        conversation: prompts, replies, one line per tool call
  --full                With --transcript, also print tool results and shell
                        command output (trimmed)
  --max-bytes N         With --transcript, the output limit, default 120000
                        (about 30k tokens); 0 for no limit. When trimmed, the
                        full version is saved in the data directory for 7 days
                        and its path is named at the end
  --json                Print JSON: the metadata sidecar, which an imported
                        session extends with origin, imported_at, and
                        started_at_source; with --transcript, then the
                        normalized conversation
  --no-pager            Print the summary or transcript directly; do not
                        page through $PAGER
  --normalized          Deprecated: the same as --transcript --json
Example: agent-archive show SESSION_ID --transcript
`,
	"stats": `Usage: agent-archive stats [options]

Show how you use your coding agents: tokens by day, sessions, estimated cost,
agents, models, projects, what the tokens were spent on, and highlights, over
the last 30 days by default, with the change from the 30 days before. Reads
metadata only; prints numbers and names, never prompts or paths. Cost is an
estimate at list price, not a bill, from a dated price table. Tokens and cost
say "unknown" for sessions that record none (Cursor). A subagent's tokens
count with its session. Sessions are placed by capture time, so imported
sessions appear on the day they were imported.
On a terminal of 80 columns or more, bars; narrower, a compact table. Text is
paged through $PAGER unless --no-pager. Not a terminal: no color, full layout.
  --days N                       Window of N calendar days ending today
                                 (default 30; up to 3660)
  --since DATE|TIME|AGE          Window from this local day through today (a
                                 date, an RFC 3339 time, or an age: 7d, 12h;
                                 a date is a local day here, not UTC as in
                                 list). Not with --days
  --by day|week|month|project    Also break the window down that way
  --harness codex|claude|cursor  Only this application
  --model NAME                   Only sessions that used this model (their
                                 other models count too)
  --imported                     Only sessions agent-archive backfill imported
  --hook-captured                Only sessions hooks captured as they ran
  --prices FILE                  Price tokens with the prices in this JSON file
                                 (the built-in table's format), applied on top
                                 of it; the output says so
  --no-cache                     Download every metadata sidecar instead of
                                 reusing the local metadata cache
  --no-pager                     Print directly; do not page through $PAGER
  --json                         Print a versioned document ({"schema_version":
                                 1, ...}) of the numbers: unknown is null,
                                 never 0. Usage errors print no JSON.
  --html                         Write one self-contained web page (inline
                                 styles and SVG; no script, no requests, works
                                 in light and dark and prints) to stdout, or
                                 to --output. Counts and names only. Not with
                                 --json
  --output FILE                  With --html, write the page to FILE (mode
                                 0600, replaced in one step) instead of stdout
  --force                        With --output, replace FILE if it is an
                                 ordinary file that exists
  --include-names                With --html, name the real projects, skills,
                                 MCP servers and models the built-in price
                                 table does not list; by default the page
                                 says project A, skill A, MCP server A,
                                 model A, ... so it can be shared
Example: agent-archive stats --since 2026-09-01 --by project
Example: agent-archive stats --html --output stats.html
`,
	"handoff": `Usage: agent-archive handoff [SESSION_ID|--latest|--file PATH] [options]

Print a session as a prompt another coding agent can continue from. This
prints conversation content, filtered as it is for the archive: injected
instructions and credentials removed, tool output trimmed, edit bodies left
out. A session on this machine is read from its transcript now, without
waiting for a sync; otherwise it is downloaded from the archive.
With no selector on a terminal, pick a session from a numbered list of this
Mac's sessions (including ones not yet uploaded) and archived ones, newest
first. Without a terminal, or when AGENT_ARCHIVE_NONINTERACTIVE is on
(automatic inside coding agents), give a SESSION_ID, --latest, or --file PATH.
On a terminal without --to, --output, or --format json, it then asks where
to continue: an installed agent (default: handoff.default_to in
config.json, else Codex for a Claude Code session and Claude Code for the
others), or print, copy to the clipboard, or write to a file. Piped, or
with AGENT_ARCHIVE_NONINTERACTIVE on, it prints the handoff as it always has.
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
  --to NAME             Launch local claude, codex, or cursor with the handoff.
                        The agent reads a copy kept in the data directory for
                        7 days (before setup, in a private temporary folder);
                        needs the agent's CLI (claude, codex, or agent) on
                        PATH. With no selector, hands off the Claude Code,
                        Codex, or Cursor session it runs in, or picks one on
                        a terminal. On a terminal the agent runs there;
                        otherwise it opens in a new tmux window, or a new
                        iTerm2, Ghostty, or Terminal tab, and the command
                        returns
  --here                Run the launched agent in this terminal
  --new-window          Open the launched agent in a new window or tab
  -- ARGS               Everything after -- goes to the launched agent, after
                        any arguments set in config.json's handoff.args
Example: agent-archive handoff
Example: claude "$(agent-archive handoff --latest --harness codex)"
Example: codex "$(agent-archive handoff --latest --harness claude)"
Example: agent-archive handoff --latest --harness claude --to codex
Example: agent-archive handoff SESSION_ID --to claude -- --model opus
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
func (e Env) newCommandFlags(command string, errOut io.Writer) *commandFlags {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	flags := &commandFlags{FlagSet: fs, errOut: errOut}
	if e.observeFlags != nil {
		e.observeFlags(flags)
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
