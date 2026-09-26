# Routine use and troubleshooting

Run `agent-archive` for a short command guide, or `agent-archive COMMAND
--help` (or `agent-archive help COMMAND`, including `help backfill undo`) for
options and examples. Help never activates hooks, reads credentials, or
changes state. Invalid flags fail before a command starts, with one line
naming the problem. Every command's options and the exit codes are in the
[CLI reference](../reference/cli.md#exit-codes). Commands that need setup and
find none say so without creating the data directory.

## Everyday commands

- `sync` collects and uploads once now, reporting results. It respects pause.
  A session is republished at most once per upload interval (3 minutes), so
  new activity soon after an upload waits; `sync` says so and when the first
  one is due:

  ```text
  Scanned 5 session(s): 0 published, 5 waiting for the upload interval (next at 14:27), 0 unchanged, 0 failed.
  ```

  Waiting work is saved locally and uploaded by the next pass after that
  time; `status` counts it under Pending.
- `pause` persists until `resume`. If work is still running, the command
  reports that no settings changed, names the command holding the collector
  lock (and its process ID), and asks you to retry after it finishes. `sync`,
  `setup`, and `uninstall` name it the same way.
- A local file that cannot be read is named, with a way out. `status` reports
  around the ones it can do without (collector status, storage health,
  capture diagnostics, a single session's records) with a warning, and the
  collector or the next hook replaces them. `setup` offers to move a damaged
  or newer-version saved setup aside (renamed `*.corrupt`), and
  `setup --abandon-recovery` does the same for a damaged recovery record. A
  damaged `config.json` stops every command, which names it; restore it from
  a backup, or move it aside and run `setup` again.
- If your home folder is itself a Git checkout (a dotfiles repository, say),
  every command stops and names it: the archive's data never goes inside a
  checkout. Set `AGENT_ARCHIVE_HOME` to a directory outside it.
- Sessions registered before a pause catch up after `resume`, including
  activity written during the pause. New sessions begun while paused are not
  imported.
- Reducing retention shows how many locally owned sessions are affected and
  the cutoff before it asks. The collector applies the new policy.

## Reading status

```sh
agent-archive status
agent-archive status --json
```

Status uses local evidence and a read-only launchd check; it never downloads
conversations. `status --json` separates configured, hook-observed, captured,
published, and read-back-verified evidence.

- **Before setup**, status shows only that setup is needed (`--json`:
  background `missing`, authentication `not_configured`).
- **Background** `loaded` means launchd knows the scheduled job; `running`
  means a pass is executing; `another_installation` means launchd runs this
  installation's label from a different plist. That job belongs to another
  installation and is left alone: set `AGENT_ARCHIVE_HOME` to a data
  directory of this installation's own, or uninstall the other one.
- **Hooks or background `broken`** means the configuration is in place but
  runs an `agent-archive` executable that has since been moved, deleted, or
  made non-executable. Rerun `agent-archive setup` from the binary's new
  location.
- **The background collector cannot load your AWS profile** (S3): the
  collector runs with the AWS files and `PATH` setup recorded in its
  LaunchAgent, and one of them no longer works: an `AWS_CONFIG_FILE` that
  moved, or a `credential_process` helper that is no longer on that `PATH`
  (a LaunchAgent from an earlier build has only launchd's
  `/usr/bin:/bin:/usr/sbin:/sbin`). `agent-archive sync` from your shell
  may still work. Run `agent-archive setup` again from a shell where the
  profile works. A warning that your shell's AWS settings files differ from
  the collector's means `sync` here and the collector read different
  profiles.
- **The background collector couldn't get credentials from your AWS
  profile's `credential_process`** (S3): the helper ran but failed. It runs
  without most of your shell's environment: aws-vault's and 1Password's
  settings for where credentials live are passed on, but other settings
  aren't
  ([configuration](../reference/configuration.md#environment-variables)).
  Put a missing setting in the helper's own configuration. A helper that
  needs you to unlock it or sign in (a locked vault, an expired `op`
  session) fails until you do. Check with `agent-archive sync`, then run
  `agent-archive setup` again from a shell where it works.
- **Capture** distinguishes waiting for a session, observed hooks, local
  capture, and published sources with verified checksums. Configuration
  alone never establishes capture.
- **Access** shows when the bucket was last confirmed reachable with the
  configured credentials, and by whom: setup's check, or the collector (its
  access probe, or a pass that uploaded). The collector checks storage with
  one synthetic round trip for a new configuration, retries failed checks,
  and refreshes the check after four minutes. **Authentication** calls a
  verified check stale after ten minutes, unless collection is paused, when
  the last check is shown with its time.
- **Capture gaps.** `status --json` counts, per app, the sessions whose
  last capture recorded a gap (`sessions_with_capture_gaps`). The fields are
  described in [JSON output](../reference/json-output.md#status---json).
- **Read-back.** Each publication is read back and compared. One that cannot
  be read back is retried with increasing delays (one minute up to a day), at
  most five per pass, oldest first; status reports it as pending, failed, or
  mismatched. It does not fail `sync`.
- **Collection stuck.** Every command that takes the collector lock records
  which command it is and when. When one has held it for over two hours
  (twice a pass's hard time limit), status says collection is stuck and names
  the command and its process ID.
- **Quarantined state.** Status counts local state files a pass could not read
  and moved aside, and session summaries this version cannot refresh.
- **Authentication** says whether its evidence came from a manual `sync` or
  the background environment. App versions a hook doesn't report stay
  unknown.

## An interrupted setup

A failed setup restores the previous configuration, hooks, and scheduler. If
setup is interrupted before it can (a crash, a closed terminal), it leaves a
record in `setup-transaction.json` in the data directory; `status` says
recovery is needed, hooks record nothing, and `sync`, `pause`, `uninstall`,
and `backfill` refuse to start.

- Run `agent-archive setup` to recover: it restores the previous files and
  continues.
- Recovery never overwrites a file edited since the interrupted setup (an app
  may rewrite its own settings file). Setup then names the file and the
  record. To keep every file as it is now and discard the record, run
  `agent-archive setup --abandon-recovery`, then `agent-archive setup` to
  review your settings and reinstall anything missing.

## Changing storage

A storage change is blocked while known work is pending; sync the current
destination first. A session that never received a transcript (a Cursor chat
with transcripts turned off) has nothing to publish, so it does not block the
change. Switching starts a new capture boundary: old sessions stay published
at their old destination, which this Mac stops collecting into or cleaning
up. Their local evidence is kept until it ages past the retention period,
then removed from this Mac only; nothing is deleted from either bucket on its
behalf.

## Upgrading from an earlier build

These notes are for builds from before the first release; a new install
can skip them.

- With S3 storage, run `agent-archive setup` again and save your settings
  unchanged; saving rewrites the collector's LaunchAgent. An
  earlier build's LaunchAgent gives the background collector none of your
  shell's AWS settings files, CA bundle, endpoints, proxy or `PATH`, so a
  profile outside `~/.aws` or one using `credential_process` (`aws-vault`,
  `op`, `granted`) uploads only when you run `sync`. `status` reports the
  cases it can see: a `credential_process` helper launchd's `PATH` doesn't
  find, or your shell's AWS files differing from the collector's.

- A source bundle written by an early build as a single JSON document
  (schema 1) is not read: `show --normalized` and `handoff` report
  "unsupported source schema version 1" for it.
- Setup retires the old `com.agent-skills.skill-runs-upload` job of the
  prototype this tool grew out of, only when its label and command match
  that prototype, and keeps the prototype's private records.
- With `AGENT_ARCHIVE_HOME` set to a non-default directory, hooks carry it in
  their command and the collector gets its own launchd label. Until you rerun
  `agent-archive setup`, `status` reports those hooks as `missing or
  incomplete`.
- Backfill keeps a local record of every session retention or undo removes,
  so a later backfill doesn't import it again. Sessions retention removed
  before that record existed can be imported once more if their transcripts
  are still on disk; check the plan, or use `--since`.
- Read-back evidence is keyed per session to the app, project root, and
  project activation time. The first run after upgrading from a build without
  that invalidates every `verification.json` and `storage-health.json` once:
  `status` reports authentication as `stale_configuration` and every
  publication as read-back pending until the next pass re-verifies it (capped
  per pass, so a large archive recovers over several passes; nothing is
  re-uploaded).
- A project that is removed and later re-added gets a fresh activation time.
  Sessions registered before that time are no longer accepted for that
  project; their earlier publications stay in the bucket until removed by
  hand.

## Something else

Reporting a bug: use the issue templates. A capture gap (a session or field
that should have been archived and wasn't) has a template of its own.
Security problems go through [SECURITY.md](../../SECURITY.md), not public
issues. See also the [FAQ](faq.md).
