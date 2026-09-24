# Uninstall

```sh
agent-archive uninstall
```

After confirmation, uninstall stops the background collector, removes its
LaunchAgent and the hook entries setup added, and disables capture. It keeps
local evidence, settings, and credentials, so `agent-archive setup` can
reinstall. Unrelated hook handlers, the bucket, and the `agent-archive`
executable are always kept. Confirming needs a terminal; `--yes` skips the
confirmations and is required without one.

Only the collector of the current data directory (see
[`AGENT_ARCHIVE_HOME`](../reference/configuration.md#environment-variables))
is stopped, and only when launchd loaded it from this installation's own
plist. The hook files of the apps setup installed must be readable,
including any an earlier release left at `~/.claude/settings.json` or
`~/.codex/hooks.json`; any other file is only checked for leftover handlers,
so one that is not valid JSON is reported and left as it is rather than
blocking uninstall.

## Delete local data too

```sh
agent-archive uninstall --delete-local-data
```

This also deletes agent-archive's own files in the data directory and stored
R2 credentials. It shows how many sessions are still waiting to upload and
asks a second time (`--yes` answers both). Unpublished evidence is lost. Only
files agent-archive creates are removed (see
[local state](../reference/local-state.md)); anything else in the directory
is kept and reported. The lock files go last, while uninstall still holds
them, so nothing can start work in a half-deleted directory.

Neither mode reads or deletes anything in the bucket. To delete archived
sessions, delete `sessions/` (under your prefix) in the bucket yourself, or
lower retention first and let the collector remove them.

## When uninstall refuses

- If another operation is finishing, wait and retry.
- If launchd is unavailable, or a hook file was edited while uninstall ran,
  resolve the reported problem and run it again.
- After an interrupted setup, uninstall refuses until `agent-archive setup`
  recovers or `agent-archive setup --abandon-recovery` discards the record
  (see [troubleshooting](../guides/troubleshooting.md#an-interrupted-setup)).

Do not remove the data directory by hand while a collector is running.

## Downgrading

- **After changing storage**, do not downgrade to a version that does not
  support `destination_since`. Older versions ignore that saved boundary and
  may upload earlier sessions to the new destination.
- **Pending metadata-only publications.** This version can queue a
  publication that carries only a reference to an uploaded source (when a
  parser upgrade meets a retained bundle it cannot rebuild). Before
  downgrading, run `agent-archive sync` until `status` shows nothing pending:
  an earlier version cannot publish such a file (under `pending/`, with
  `"metadata_only": true` and no `source_bytes`) and reports it as failing on
  every pass. If you already downgraded, deleting those files is safe; the
  metadata is refreshed on the session's next change.
