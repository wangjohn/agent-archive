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
  agent-archive show        Read a session's metadata
  agent-archive feedback    Add explicit feedback from a local file

Import history
  agent-archive backfill    Import sessions already on this Mac

Switch agents
  agent-archive handoff     Continue a session in another coding agent

Maintenance
  agent-archive uninstall   Remove integrations; keep local data

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
Usage: agent-archive setup [--abandon-recovery]
       agent-archive setup --yes [--provider r2|s3 ...] [--project DIR ...]

Choose apps and projects, connect storage, then review and enable capture.
Run again to continue saved setup or edit capture, storage, or retention.
Credentials are entered privately; never pass them as command arguments.
Setup asks questions, so it needs a terminal, unless --yes is given.
An interrupted setup is recovered on the next run.
  --abandon-recovery    If recovery stops because a file it changed was
                        edited since, keep every file as it is now and
                        discard the interrupted setup; then run setup again
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
| `--project` | a value | — |
| `--provider` | a value | — |
| `--r2-access-key-id` | a value | — |
| `--r2-account` | a value | — |
| `--region` | a value | — |
| `--yes` | no value | — |

## agent-archive status

Guide: [Reading status](../guides/troubleshooting.md#reading-status); `--json` fields in [JSON output](json-output.md).

```text
Usage: agent-archive status [--json]

Show local capture evidence, background health, and a next step.
No conversations are printed and no cloud request is made.
--json prints the same status as a versioned JSON document. In it,
storage_verified_at is when setup's storage check last passed, and
storage_access_confirmed_at is the latest confirmation of access (by
"setup" or the "collector", in storage_access_confirmed_by).
Example: agent-archive status --json
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--json` | no value | — |

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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--complete` | no value | — |
| `--harness` | a value | — |
| `--hook-captured` | no value | — |
| `--imported` | no value | — |
| `--json` | no value | — |
| `--model` | a value | — |
| `--no-cache` | no value | — |
| `--since` | a value | — |
| `--skill` | a value | — |
| `--skill-sha256` | a value | — |
| `--skill-usage` | a value | `used` |

## agent-archive show

Guide: [Inspect the archive](../guides/list-and-show.md).

```text
Usage: agent-archive show SESSION_ID [--harness NAME] [--normalized] [--json]

Print session metadata as JSON (--json is accepted, as for list and status).
An imported session also shows origin, imported_at, and started_at_source.
  --harness NAME        The session's app, if the same SESSION_ID exists under
                        more than one
  --normalized          Also download and verify the source bundle, and print
                        conversation content as well
  --json                JSON, the default and only format
Example: agent-archive show SESSION_ID --normalized
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--harness` | a value | — |
| `--json` | no value | — |
| `--normalized` | no value | — |

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
Usage: agent-archive handoff SESSION_ID|--latest|--file PATH [options]

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
```

| Flag | Takes | Default |
| --- | --- | --- |
| `--file` | a value | — |
| `--force` | no value | — |
| `--format` | a value | `markdown` |
| `--harness` | a value | — |
| `--latest` | no value | — |
| `--max-bytes` | a value | `120000` |
| `--no-preamble` | no value | — |
| `--output` | a value | — |
| `--project` | a value | — |
| `--source` | a value | `auto` |

## agent-archive uninstall

Guide: [Uninstall](../getting-started/uninstall.md).

```text
Usage: agent-archive uninstall [--delete-local-data] [--yes]

Remove hooks and the background collector. Keep local evidence, settings,
and credentials by default, so setup can restore the installation.
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

## agent-archive version

Guide: [Install](../getting-started/install.md).

```text
Usage: agent-archive version

Print the installed version (also: agent-archive --version, -v).
Example: agent-archive --version
```
