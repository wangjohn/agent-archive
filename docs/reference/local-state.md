# Local state

agent-archive keeps its local state in one data directory:
`~/.local/share/agent-archive`, or the directory `AGENT_ARCHIVE_HOME` names
(see [configuration](configuration.md#environment-variables)). The directory
must not be inside a Git checkout. Nothing here is uploaded as a file; what is
uploaded is built from it and described in [bucket layout](bucket-layout.md).
Files are private to your account and written atomically (temporary file,
fsync, rename, directory fsync).

R2 credentials are not here: they are in the macOS Keychain under the service
`agent-archive`. S3 credentials stay in your AWS profile.

## Who owns what

- **`internal/state`** owns the session files: registrations, requests,
  published and pending publications, change detection, the native-session
  index, subagent candidates, removal records, refresh-skips, and
  `status.json`. Its `Store` holds the rules for each file and the
  per-session locks (`request-locks/`) that keep a hook and the collector
  from interleaving; `state.OwnedEntries()` lists every top-level entry it can
  create. It never reads transcripts or talks to storage.
- **`internal/config`** owns `config.json`; **`internal/cli`** owns setup's
  files, logs, diagnostics, the collector-lock record, and `handoffs/`;
  **`internal/backfill`** owns `imports/`; **`internal/reader`** owns
  `cache/`.

## Configuration and setup

| Entry | What it is |
| --- | --- |
| `config.json` | The configuration: storage, projects, apps, retention, pause. See [configuration](configuration.md). |
| `setup-draft.json` | Setup's saved progress, so an interrupted setup can continue. Holds no secrets. |
| `setup-transaction.json` | Present only while setup is changing files, or after it was interrupted; see [troubleshooting](../guides/troubleshooting.md#an-interrupted-setup). |
| `application-versions.json` | Installed app versions setup found. |

## Sessions (`internal/state`)

| Entry | What it is |
| --- | --- |
| `registrations/<id>.json` | One per captured session: archive session ID, native ID, project, app, transcript path or Cursor chat ID, start and admission times, origin (hook or import), destination. |
| `requests/<id>.json`, `request-locks/` | Hook evidence waiting for the next collector pass, and the per-session locks around it. |
| `sessions/<hash>.json` | Index from a native session ID (hashed, since it is app-controlled input) to its archive session ID. |
| `sessions/<id>/` | Per-session evidence, such as `verification.json` (the last read-back check). |
| `subagent-candidates/` | Subagent transcripts a hook reported, waiting for the collector to validate them. |
| `pending-scans/`, `scan-signatures/` | Transcript reads in progress, and what each transcript looked like at its last read, so an unchanged one isn't read again. |
| `pending/<id>.json` | A publication frozen before its first upload: the exact source and metadata bytes, so retries are byte-identical. |
| `published/<id>.json` | The last bundle built and the last one published, with the uploaded source's key, SHA-256, and size, and its metadata. |
| `superseded/<id>.json` | Earlier source objects of the session, oldest first, which retention deletes after a grace period. |
| `refresh-skips/<id>.json` | A session whose metadata this build can't refresh (`metadata_underivable`, or `source_unavailable` when the recorded source is gone), with the parser version and source key it applies to. It is not retried until either changes; `status` counts these as unrefreshable summaries. |
| `forgotten/<hash>.json` | A record of each session retention or undo removed, so backfill doesn't import it again. |
| `status.json` | The collector's last pass: scan and publish times, counts, errors. |

**Quarantine.** A session file a pass can't parse at all (truncated, empty,
not JSON) is re-read under its lock, then renamed
`<name>.json.<UTC time>.corrupt` and reported for that session only, so it
stops failing every pass. It is kept for you to inspect; `status --json`
lists it under `quarantined_files` until you delete it. A file of valid JSON
but an unexpected shape (for example one a newer version wrote) is reported
and left in place.

## Everything else

| Entry | Owner | What it is |
| --- | --- | --- |
| `imports/<id>.json` | backfill | One per import. See [backfill](../guides/backfill.md#import-files). |
| `storage-health.json` | cli | The last storage access check. |
| `capture-diagnostics.json`, `diagnostics.lock` | cli | Content-free records of sessions a hook declined, for `status`. |
| `collector.log`, `collector-error.log` | cli | The background collector's output. The error log is trimmed in place to its most recent part once it grows past a limit. |
| `collector-lock.json` | cli | Which command holds `collector.lock`, its process ID, and since when; `status` uses it to report stuck collection. |
| `cache/` | reader | A disposable cache of metadata sidecars for `list`. Safe to delete. |
| `handoffs/` | cli | Untrimmed handoffs saved when output was trimmed; removed after 7 days. |
| `collector.lock`, `hooks.lock`, `setup.lock` | local | File locks coordinating processes. |

## What uninstall removes

`agent-archive uninstall --delete-local-data` removes every entry above,
the three lock files last: it holds them while deleting everything else, so
no process can start work in a half-deleted directory, then unlinks them
before releasing them. It also removes leftover temporary files from
interrupted writes. Anything else in the directory is kept and reported, and
the directory itself stays if anything is left. A test keeps uninstall's
list in step with `state.OwnedEntries()`.

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
