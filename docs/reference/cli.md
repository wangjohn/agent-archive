# CLI reference

<!-- Generated from the commands' own help and flag sets. Do not edit by
     hand; after changing a command's help or flags, regenerate it with
     go test ./internal/cli -run TestCLIReferenceIsCurrent -update -->

Every public command of `agent-archive`: its help, exactly as
`agent-archive help COMMAND` (or `agent-archive COMMAND --help`) prints it,
the flags its parser accepts, and a link to the guide that walks through it.
New to agent-archive? Start with [install](../getting-started/install.md) and
[setup](../getting-started/setup.md); the [glossary](glossary.md) explains
terms such as capture, collector, and retention.

Every command also accepts `--help` and `-h`. Help never activates
hooks, reads credentials, or changes state. The hidden commands `_hook` and
`_collect` (what app hooks and the LaunchAgent run) are not part of the
interface and are left out.

This page is generated from the CLI itself, and `go test ./...` fails when it
is stale. After changing a command's help or flags, regenerate it with
`go test ./internal/cli -run TestCLIReferenceIsCurrent -update` (see
[fixtures and goldens](../../dev/contributing/testing.md#fixtures-and-goldens)).

## Commands

```text
Agent Archive — archive coding-agent sessions to your private storage.

Get started
  agent-archive setup       Configure apps, projects, and storage
  agent-archive status      Check capture and see what to do next

Manage capture
  agent-archive sync        Collect and upload pending changes now
  agent-archive pause       Pause collection, uploads, and cleanup
  agent-archive resume      Resume automatic capture

Inspect history
  agent-archive list        Find archived sessions
  agent-archive show        Read a session's summary or transcript
  agent-archive stats       See your usage: tokens, cost, agents, projects
  agent-archive feedback    Add explicit feedback from a local file

Import history
  agent-archive backfill    Import sessions already on this Mac

Switch agents
  agent-archive handoff     Continue a session in another coding agent

Maintenance
  agent-archive uninstall   Remove integrations; keep local data
  agent-archive purge       Review and remove unreferenced source objects

Run agent-archive COMMAND --help for options and examples.
Use --version to show the installed version.
You supply a private Cloudflare R2 or Amazon S3 bucket. No archive account needed.
Docs: https://github.com/wangjohn/agent-archive/tree/main/docs
```

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, and help. |
| 1 | An operational failure. What a command was asked for goes to stdout; why it did not do it, or not all of it, goes to stderr. A `sync` that is paused, finds another command running, or fails for some sessions exits 1; its summary line, if it ran, stays on stdout. |
| 2 | A usage error: an unknown command or flag, a bad flag value, or an unexpected argument. It is reported in one line on stderr before the command does anything. |
| 128 + signal | `backfill` stopped at once by a second Ctrl-C (130), SIGHUP (129), or SIGTERM (143). |

## agent-archive setup

Guide: [Set up capture](../getting-started/setup.md).

```text
Usage: agent-archive setup [--abandon-recovery] [--verbose]
               [--no-skills | --skills]
       agent-archive setup --yes [--provider r2|s3 ...] [--project DIR ...]
               [--skill-evidence none|metadata|body] [--no-skills | --skills]

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
  --no-skills           Install no agent skills (such as /handoff), and
                        remove those setup wrote. Later setup runs keep
                        them off until --skills
  --skills              Turn the agent skills back on and install them
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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--abandon-recovery` | no value | — |
| `--apps` | a value | — |
| `--aws-profile` | a value | — |
| `--bucket` | a value | — |
| `--no-skills` | no value | — |
| `--project` | a value | — |
| `--provider` | a value | — |
| `--r2-access-key-id` | a value | — |
| `--r2-account` | a value | — |
| `--region` | a value | — |
| `--skill-evidence` | a value | — |
| `--skills` | no value | — |
| `--verbose` | no value | — |
| `--yes` | no value | — |

## agent-archive status

Guide: [Reading status](../guides/troubleshooting.md#reading-status); `--json` fields in [JSON output](json-output.md).

```text
Usage: agent-archive status [APP] [--verbose] [--json] [--no-pager]

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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--json` | no value | — |
| `--no-pager` | no value | — |
| `--verbose` | no value | — |

## agent-archive sync

Guide: [Everyday commands](../guides/troubleshooting.md#everyday-commands).

```text
Usage: agent-archive sync

Collect and upload once. Failures leave pending work available to retry.
If paused, no work is started; run agent-archive resume first.
Example: agent-archive sync
```

No flags.

## agent-archive pause

Guide: [Everyday commands](../guides/troubleshooting.md#everyday-commands).

```text
Usage: agent-archive pause

Persistently pause collection, uploads, and cleanup. Current work must finish
before the command can confirm pause; retry if another operation is running.
Already registered sessions can catch up after resume, including activity
written during the pause. New sessions begun while paused are not imported.
Example: agent-archive pause
```

No flags.

## agent-archive resume

Guide: [Everyday commands](../guides/troubleshooting.md#everyday-commands).

```text
Usage: agent-archive resume

Resume scheduled collection, uploads, and cleanup. Already registered sessions
can catch up, including activity written during the pause. For an immediate
pass, run agent-archive sync.
Example: agent-archive resume
```

No flags.

## agent-archive list

Guide: [Inspect the archive](../guides/list-and-show.md); `--json` in [JSON output](json-output.md).

```text
Usage: agent-archive list [options]

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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--complete` | no value | — |
| `--harness` | a value | — |
| `--hook-captured` | no value | — |
| `--imported` | no value | — |
| `--json` | no value | — |
| `--limit` | a value | `50` |
| `--model` | a value | — |
| `--no-cache` | no value | — |
| `--no-pager` | no value | — |
| `--rebuild-index` | no value | — |
| `--since` | a value | — |
| `--skill` | a value | — |
| `--skill-sha256` | a value | — |
| `--skill-usage` | a value | `used` |
| `--verbose` | no value | — |

## agent-archive show

Guide: [Inspect the archive](../guides/list-and-show.md).

```text
Usage: agent-archive show [SESSION_ID|TITLE] [options]

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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--full` | no value | — |
| `--harness` | a value | — |
| `--json` | no value | — |
| `--max-bytes` | a value | `120000` |
| `--no-pager` | no value | — |
| `--normalized` | no value | — |
| `--transcript` | no value | — |

## agent-archive stats

Guide: [See your usage](../guides/stats.md); `--json` in [JSON output](json-output.md).

```text
Usage: agent-archive stats [options]

Show how you use your coding agents. The default screen is a summary of the
last 30 days: estimated spend, sessions and tokens (with the change from the
30 days before), which agents did the work, daily spend, where it went by
project and model, the skills and MCP servers used most, and anything worth a
look. Reads metadata only; prints numbers and names, never prompts or paths.
Cost is an estimate at list price, not a bill, from a dated price table.
Tokens and cost say "unknown" for sessions that record none (Cursor). A
subagent's tokens count with its session. Sessions are placed by capture
time, so imported sessions appear on the day they were imported.
Two columns of bars from 80 terminal columns, one column from 60, a compact
table below that. Color on a color terminal (16 ANSI colors; NO_COLOR turns it
off). Not a terminal: no color, full layout.
On a terminal (stdin and stdout) it opens an interactive screen, unless
--view, --detail, --by, --no-pager, --json or --html is given, or
AGENT_ARCHIVE_NONINTERACTIVE is on, as it is inside coding agents: o d p m a
switch views, w cycles the window (7d, 30d, 90d, or --days and --since as a
custom one) without reading again, arrows, j, k, PgUp, PgDn, space, Home and
End scroll, h saves the redacted page as HTML (it asks for a file name and
never replaces one), ? lists the keys, q quits. Otherwise text is paged
through $PAGER unless --no-pager.
  --view overview|detail|projects|models|agents
                                 Which screen to print (default overview):
                                 detail has the streaks, tool errors, token
                                 breakdown, every skill and MCP server and the
                                 notes on what the numbers rest on; projects,
                                 models and agents list every one. Not with
                                 --json or --html
  --detail                       The same as --view detail. Not with --view
  --days N                       Window of N calendar days ending today
                                 (default 30; up to 3660)
  --since DATE|TIME|AGE          Window from this local day through today (a
                                 date, an RFC 3339 time, or an age: 7d, 12h;
                                 a date is a local day here, not UTC as in
                                 list). Not with --days
  --by day|week|month|project    Also break the window down that way:
                                 project is --view projects, and day, week
                                 and month add a table to --view detail
                                 (--json has the rows for all four)
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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--by` | a value | — |
| `--days` | a value | `30` |
| `--detail` | no value | — |
| `--force` | no value | — |
| `--harness` | a value | — |
| `--hook-captured` | no value | — |
| `--html` | no value | — |
| `--imported` | no value | — |
| `--include-names` | no value | — |
| `--json` | no value | — |
| `--model` | a value | — |
| `--no-cache` | no value | — |
| `--no-pager` | no value | — |
| `--output` | a value | — |
| `--prices` | a value | — |
| `--since` | a value | — |
| `--view` | a value | — |

## agent-archive feedback

Guide: [Feedback](../guides/list-and-show.md#feedback).

```text
Usage: agent-archive feedback SESSION_ID --file PATH

Attach an explicit user assessment to a locally captured session. The file is
read locally, privacy-filtered, and queued for the next collection pass. Its
path is not archived. While capture is paused, it waits until you resume.
Example: agent-archive feedback SESSION_ID --file /private/path/feedback.txt
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--file` | a value | — |

## agent-archive backfill

Guide: [Import existing sessions](../guides/backfill.md).

```text
Usage: agent-archive backfill [options]
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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--background` | no value | — |
| `--dry-run` | no value | — |
| `--harness` | a value; repeatable | — |
| `--include-home` | no value | — |
| `--include-removed` | no value | — |
| `--include-temp` | no value | — |
| `--json` | no value | — |
| `--project` | a value; repeatable | — |
| `--since` | a value | — |
| `--until` | a value | — |
| `--yes` | no value | — |

## agent-archive backfill history

Guide: [Import existing sessions](../guides/backfill.md).

```text
Usage: agent-archive backfill history

List past imports, oldest first: each import's IMPORT_ID, when it started,
how many sessions and projects it added, and its upload state (waiting,
uploaded, interrupted, or undone). Reads this Mac's records only.
Example: agent-archive backfill history
```

No flags.

## agent-archive backfill undo

Guide: [Undo an import](../guides/backfill.md#undo).

```text
Usage: agent-archive backfill undo [IMPORT_ID] [--project DIR] [--yes]
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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--project` | a value | — |
| `--restore-retention` | no value | — |
| `--yes` | no value | — |

## agent-archive handoff

Guide: [Continue a session in another agent](../guides/handoff.md).

```text
Usage: agent-archive handoff [SESSION_ID|TITLE|--latest|--file PATH] [options]

Continue a session in another coding agent. On a terminal, pick a session
(this Mac's, including ones not yet uploaded, and archived ones), then pick
where to continue: an installed agent starts in this terminal with the
session as its context (Enter takes handoff.default_to in config.json, else
Codex for a Claude Code session and Claude Code for the others), or print,
copy to the clipboard, or write to a file. Inside Claude Code, /handoff codex
runs handoff --to codex, which opens Codex in a new terminal tab or window.
The session is filtered as it is for the archive: injected instructions and
credentials removed, tool output trimmed, edit bodies left out. A session on
this Mac is read from its transcript now; otherwise it is downloaded from
the archive. Piped, or with --output, --format json, or --no-preamble, it
prints without asking. Without a terminal, give a SESSION_ID or TITLE,
--latest, or --file PATH (or --to, from inside an agent). Inside a coding
agent, or with AGENT_ARCHIVE_NONINTERACTIVE=1, it never asks, even on a
terminal.
A TITLE substring or short SESSION_ID matches as it does for show, in this
Mac's sessions first (no network), then the archive's; a full SESSION_ID
wins. Quote a title of several words. Several matches on a terminal open the
picker on them; without one, or inside a coding agent, they are listed on
stderr and the command exits 1, never guessing. A title skips the agent
session running the command, unless --to is given.
  --latest              The most recent session for the project
  --project DIR         Project for --latest, and where the agent starts
                        (default: current directory)
  --harness NAME        claude, codex, or cursor
  --file PATH           Render a native transcript directly (needs --harness);
                        works for sessions the archive never captured
  --source auto|local|archive
                        Where the content comes from: auto (default) reads
                        this Mac's transcript when there is one, else the
                        archive; local or archive uses only that one
  --max-bytes N         Output limit, default 120000 (about 30k tokens); 0 for
                        no limit. When trimmed, the full version is saved for
                        7 days and its path is named at the end
  --format markdown|json
                        markdown (default) prints the prompt; json prints
                        the structured handoff document it is rendered from
  --output FILE         Write to FILE (mode 0600) instead of printing it
  --force               With --output, replace FILE if it exists
  --no-preamble         Omit the note addressed to the receiving agent
  --to NAME             Start claude, codex, or cursor without asking (its
                        CLI must be on PATH). It reads a private copy of the
                        handoff, kept 7 days. With no session named, hands
                        off the agent session it runs in, else picks one on
                        a terminal. On a terminal the agent runs there;
                        otherwise, or inside a coding agent, it opens in a
                        new tmux window, iTerm2 or Ghostty tab, or Terminal
                        window, and the command returns. If the session was
                        active in this checkout in the last 2 minutes, a
                        terminal asks first (inside an agent it only warns)
  --here                Run the launched agent in this terminal
  --new-window          Open the launched agent in a new window or tab
  --worktree            Start the agent in a new git worktree beside the
                        checkout, on a new branch at HEAD, carrying
                        uncommitted changes and untracked (not ignored) files
  --branch NAME         With --worktree, the new branch (default: handoff/
                        and the first 8 characters of SESSION_ID)
  -- ARGS               Everything after -- goes to the launched agent, after
                        any arguments set in config.json's handoff.args
Example: agent-archive handoff
Example: agent-archive handoff --to codex
Example: agent-archive handoff "fix the auth bug" --harness codex --to claude
Example: codex "$(agent-archive handoff --latest --harness claude)"
Example: claude "$(agent-archive handoff --latest --harness codex)"
Example: agent-archive handoff SESSION_ID --to claude --worktree
Example: agent-archive handoff SESSION_ID --to codex -- --model o3
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--branch` | a value | — |
| `--file` | a value | — |
| `--force` | no value | — |
| `--format` | a value | `markdown` |
| `--harness` | a value | — |
| `--here` | no value | — |
| `--latest` | no value | — |
| `--max-bytes` | a value | `120000` |
| `--new-window` | no value | — |
| `--no-preamble` | no value | — |
| `--output` | a value | — |
| `--project` | a value | — |
| `--source` | a value | `auto` |
| `--to` | a value | — |
| `--worktree` | no value | — |

## agent-archive uninstall

Guide: [Uninstall](../getting-started/uninstall.md).

```text
Usage: agent-archive uninstall [--delete-local-data] [--yes]

Remove hooks, the /handoff skill, and the background collector. Keep local
evidence, settings, and credentials by default, so setup can restore the
installation.
--delete-local-data also removes owned local files and stored credentials,
including unpublished evidence, after a separate confirmation.
--yes skips the confirmations; it is required without a terminal.
Remote archives and unrelated files are always kept.
Example: agent-archive uninstall
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--delete-local-data` | no value | — |
| `--yes` | no value | — |

## agent-archive purge

Guide: [Privacy cleanup](../security/privacy.md#after-a-filter-upgrade).

```text
Usage: agent-archive purge plan [--mode unreferenced|old-filter]
       [--before-filter VERSION] [--no-pager]
       agent-archive purge apply PLAN [--yes]

Create a private five-minute deletion plan, then review its exact keys.
Plan lists still-current older-filter sessions separately and never proposes
their current sources for deletion. Pause every Mac uploading to this prefix
before apply. A versioned bucket keeps noncurrent versions and delete markers.
```

No flags.

## agent-archive purge apply

Guide: [Privacy cleanup](../security/privacy.md#after-a-filter-upgrade).

```text
Usage: agent-archive purge apply PLAN [--yes]

Pause every uploading Mac first. Confirm the plan digest or use --yes.
Apply rechecks remote metadata before each deletion and writes a resumable
report next to the plan. A plan expires five minutes after creation.
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--yes` | no value | — |

## agent-archive purge plan

Guide: [Privacy cleanup](../security/privacy.md#after-a-filter-upgrade).

```text
Usage: agent-archive purge plan [--mode unreferenced|old-filter]
       [--before-filter VERSION] [--no-pager]

Read metadata and list source objects without deleting anything. Old-filter
mode selects only unreferenced sources whose filter version is below VERSION;
both modes report still-current older-filter sessions separately. On a
terminal, the plan is paged through $PAGER unless --no-pager.
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--before-filter` | a value | — |
| `--mode` | a value | `unreferenced` |
| `--no-pager` | no value | — |

## agent-archive version

Guide: [Install](../getting-started/install.md).

```text
Usage: agent-archive version

Print the installed version (also: agent-archive --version, -v).
Example: agent-archive --version
```
