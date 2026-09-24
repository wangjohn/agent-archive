# Local state

agent-archive keeps its local state in one data directory:
`~/.local/share/agent-archive`, or the directory `AGENT_ARCHIVE_HOME` names
(see [configuration](configuration.md#environment-variables)). The directory
must not be inside a Git checkout. Nothing here is uploaded as a file; what
is uploaded is built from it and described in
[bucket layout](bucket-layout.md). Files are written atomically (temporary
file, fsync, rename, directory fsync) and are private to your account.

R2 credentials are not here: they are in the macOS Keychain under the service
`agent-archive`. S3 credentials stay in your AWS profile.

`agent-archive uninstall --delete-local-data` removes exactly the entries
below, and reports anything else it finds in the directory. The collector's
own list of entries is `collector.OwnedEntries`, and a test keeps the two
lists in step.

## Configuration and setup

| Entry | What it is |
| --- | --- |
| `config.json` | The configuration: storage, projects, apps, retention, pause. See [configuration](configuration.md). |
| `setup-draft.json` | Setup's saved progress, so an interrupted setup can continue. Holds no secrets. |
| `setup-transaction.json` | Present only while setup is changing files, or after it was interrupted; see [troubleshooting](../guides/troubleshooting.md#an-interrupted-setup). |
| `application-versions.json` | Installed app versions setup found. |

## Capture

| Entry | What it is |
| --- | --- |
| `registrations/<id>.json` | One per captured session: its archive session ID, native ID, project, app, transcript path or Cursor chat ID, start and admission times, origin (hook or import), and destination. |
| `sessions/<hash>.json` | Index from a native session ID (hashed, since it is app-controlled input) to its archive session ID. |
| `sessions/<id>/` | Per-session evidence, such as `verification.json` (the last read-back check). |
| `requests/`, `request-locks/` | Work a hook queued for the collector, and the per-session locks around it. |
| `subagent-candidates/` | Subagent transcripts a hook reported, waiting for the collector to validate them. |
| `pending-scans/`, `scan-signatures/` | Transcript reads in progress, and what each transcript looked like at its last read, so an unchanged one isn't read again. |
| `pending/<id>.json` | A publication being uploaded: the exact source and metadata bytes, frozen before the first remote write so retries are byte-identical. |
| `published/<id>.json` | What was last published for each session, and the source it references. |
| `superseded/<id>.json` | Earlier source objects of each session, oldest first, which retention deletes after a grace period. |
| `refresh-skips/` | Sessions whose metadata this version can't refresh, and why. |
| `forgotten/<hash>.json` | A record of each session retention or undo removed, so backfill doesn't import it again. |
| `imports/<id>.json` | One per backfill import. See [backfill](../guides/backfill.md#import-files). |

A state file a pass cannot read is renamed aside with a `.corrupt` suffix
(quarantined) so it stops failing every pass; `status` counts them.

## Status, logs, and caches

| Entry | What it is |
| --- | --- |
| `status.json` | The collector's last pass: scan and publish times, counts, errors. |
| `storage-health.json` | The last storage access check. |
| `capture-diagnostics.json`, `diagnostics.lock` | Content-free records of sessions a hook declined, for `status`. |
| `collector.log`, `collector-error.log` | The background collector's output. The error log is rotated. |
| `cache/` | A disposable cache of metadata sidecars for `list`. Safe to delete. |
| `handoffs/` | Untrimmed handoffs saved when output was trimmed; removed after 7 days. |

## Locks

`collector.lock` (with `collector-lock.json`, naming the command holding it),
`hooks.lock`, and `setup.lock` coordinate processes. They are small and are
kept even by `--delete-local-data`.

## Outside the data directory

- `~/Library/LaunchAgents/com.agent-archive.collector.plist` (or
  `com.agent-archive.collector.<hash>.plist` for a non-default data
  directory).
- The hook entries in each app's hook file, recorded in `hook_files` in
  `config.json`.
- A Cursor database copy, only while a read of it is in progress with Cursor
  running: `agent-archive-cursor-<uid>/cursor-snapshot-*` in your per-user
  temporary folder (`getconf DARWIN_USER_TEMP_DIR`), removed when the read
  ends. An abandoned copy is swept by the next read.
