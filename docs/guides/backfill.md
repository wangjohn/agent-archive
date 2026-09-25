# Import existing sessions (backfill)

`agent-archive backfill` imports the Claude Code, Codex, and Cursor sessions
already on this Mac. Run it after [setup](../getting-started/setup.md). With
no options it imports every session it finds, but first it shows each project
with its session count per app, what it will skip and why, and the date
retention will delete the imported sessions. Nothing changes until you answer
`y`.

```sh
agent-archive backfill --dry-run     # see the plan; changes nothing
agent-archive backfill --dry-run --json
agent-archive backfill               # import, after confirming
agent-archive backfill history       # past imports
agent-archive backfill undo          # remove the latest import from the bucket
agent-archive backfill undo ID --project DIR
```

Ctrl-C while the plan is being made stops it and changes nothing. Ctrl-C
during registration or upload stops after the session in flight; run
`backfill` again with the same options to finish the import, or let the
background collector upload what is registered. Each says at once that it is
stopping. A second Ctrl-C, or closing the terminal (SIGHUP) or SIGTERM,
quits at once, after removing the private copy of Cursor's database backfill
may have made; a copy a killed backfill leaves is removed by the next
`backfill` command of any kind.

## What gets imported

- Projects the import adds are captured from then on, like projects you
  include in setup. Setup asks once whether to keep them.
- Codex and Cursor sessions import even without their hooks installed. New
  sessions from those apps are captured only after you add them in setup.
- Cursor chats that exist only in Cursor's own database (older chats, and
  those without a transcript file) are imported too. Backfill reads the
  database without changing it; while Cursor is running it reads a private
  copy in your per-user temporary folder and deletes the copy when it is
  done. The collector keeps capturing such a chat from the database
  afterwards. Their subagent chats are not imported yet; the plan says how
  many there are.
- Sessions run from your home directory or a temporary directory are skipped
  unless you pass `--include-home` or `--include-temp`.
- A plain folder that isn't a repository (say `~/code`, where you once ran an
  agent) becomes a project that captures new sessions anywhere under it
  that no nearer project owns; the plan says so under its row. Repositories
  and app folders inside it are added to setup as excluded projects, so they
  stay out of capture as before, and the plan lists them. Include one in
  setup to capture it; undoing the import removes them again.
- `--since` and `--until` take a date (`2026-09-01`), an RFC 3339 time, or an
  age (`30d`, `12h`). Backfill selects whole local days, so a time or an age
  selects from the start of the day it falls on. An interrupted import run
  with an age is finished by the same age on a later day too. When the
  latest import was interrupted and a run with other options would start a
  new one, the plan says so and prints the command that finishes it.
- Retention applies to imports from the day they are imported, so a whole
  import expires on one day. Choose `edit` at the prompt to keep them longer.
  Retention applies to the whole archive, so `edit` only raises it; shorten
  it in setup. Undoing the import puts the earlier retention back, and the
  undo plan says how many older sessions that then deletes.
- Claude Code deletes its own transcripts after 30 days by default. To keep
  more history for backfill, set `"cleanupPeriodDays"` in
  `~/.claude/settings.json`.

The rules for which sessions are eligible are in
[session eligibility](../reference/session-eligibility.md); the full design
is in the [backfill design](../design/backfill.md).

## Undo

`backfill undo [ID]` deletes an import's sessions from the bucket, forgets
them on this Mac, and keeps later backfills from importing them again unless
you pass `--include-removed`. With `--project DIR` it undoes only that
project's sessions. The plan shows exactly what will be deleted, including
sessions resumed since the import, before it asks.

Undo also excludes the projects the import added, with two exceptions the
plan names:

- A project another import still has sessions in stays included, so that
  import's sessions keep updating. The undo of the last of those imports
  excludes it; the plan lists such a project apart from the ones the import
  added.
- A project setup included again after any undo excluded it is left
  included, whichever import added it.

Sessions captured by hooks in an excluded project stop uploading; they are not
deleted. Undo refuses, and changes nothing, when it cannot tell its sessions
from another import's (an earlier import whose file is missing had the same
ID).

## Import files

Each import is recorded in `imports/<id>.json` in the data directory
([local state](../reference/local-state.md)). If one becomes unreadable,
backfill and undo stop and name it. Repair the file. Moving it out of the
folder lets backfill run again, but that import's sessions stay archived and
`backfill undo` can no longer remove them.

An imported session whose transcript disappears before its first upload
blocks a storage destination change until retention removes it, as a
hook-captured session does.
