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

Neither mode reads or deletes anything in the bucket; see
[delete the archive](#delete-the-archive-in-the-bucket).

## Remove the binary

Uninstall keeps the `agent-archive` executable, so you can still read the
archive or set it up again. To remove it too, after uninstalling:

```sh
rm "$(command -v agent-archive)"    # add sudo if it is in /usr/local/bin and not yours
```

## Delete the archive in the bucket

**Only the Mac that captured a session ever deletes it**, through retention
or `backfill undo`, and only while that Mac runs agent-archive and isn't
paused. The sessions of a Mac you uninstall, wipe, retire, or pause for good
stay in the bucket indefinitely, and nothing else cleans them up.

To remove only unreferenced source snapshots from the currently configured
bucket, run `agent-archive purge plan`, review its exact keys, pause every Mac
uploading to that prefix, then run `agent-archive purge apply PLAN` within five
minutes. Its report lists deleted and remaining keys. This does not delete
current sessions or empty an entire bucket. The full-prefix shell procedure
below is still needed for complete archive removal.

To delete everything under your prefix now, pause or uninstall **every** Mac
that uploads there. Use the [cleanup preparation block](../security/privacy.md#after-a-filter-upgrade)
first in a bash or zsh shell. It defines `purge_prepare` and `purge_apply`,
lists and validates all metadata, and shows an initial unreferenced-source
plan without deleting it. The AWS CLI and `jq` must be installed, and your
credentials need list, read, and delete access. In that same shell, set your
bucket and prefix and make a fresh full-prefix plan:

<!-- purge-recipe:all -->
```sh
bucket=my-archive-bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty for the whole bucket
purge_prepare all
```

Review **every printed key**. An empty prefix plans the entire bucket. If the
plan contains exactly what you intend, run `purge_apply` within five minutes
in the same shell. A failed listing or metadata read means zero deletions;
rerun `purge_prepare all` after fixing it. A delete failure reports the keys
already removed and those still pending. There is no rollback. External
writers can race these shell commands, so keep all uploading Macs paused
throughout.

To delete only one Mac's sessions, find its machine ID first (`jq -r .machine_id config.json`
in that Mac's data directory, or the `machine_id` that `show` prints for one
of its sessions). Pause **every** uploading Mac, run the linked preparation
block above in the same shell, and make a new machine plan:

<!-- purge-recipe:machine (scripts/test_purge_recipe.py runs this block) -->
```sh
bucket=my-archive-bucket
prefix=agent-archive/          # your prefix with its trailing slash, or empty
machine=0123456789abcdef0123456789abcdef
purge_prepare machine "$machine"
```

Review the exact keys, then run `purge_apply` in the same shell within five
minutes. It rechecks the complete listing and every metadata object before
the first delete; a changed object aborts the plan. On partial failure, make
a new plan and inspect what remains. On a versioned S3 bucket, ordinary
deletion hides current versions; remove noncurrent versions separately or
use a lifecycle rule.

### A lifecycle rule as a backstop

A bucket lifecycle rule deletes objects by age whatever happens to your
Macs. Make it comfortably longer than the longest retention any Mac uses
(90 days by default), so it only removes what no Mac is left to delete; a
rule shorter than retention would delete sessions agent-archive still keeps.
It needs an administrator's credentials, not the archive's:

```sh
# Replaces the bucket's whole lifecycle configuration: merge any rules it already has.
aws s3api put-bucket-lifecycle-configuration --bucket my-archive-bucket \
  --lifecycle-configuration '{"Rules": [{"ID": "agent-archive-backstop", "Status": "Enabled",
    "Filter": {"Prefix": "agent-archive/"}, "Expiration": {"Days": 180}}]}'
```

For R2, add a rule for the prefix under the bucket's **Settings → Object
lifecycle rules** in the Cloudflare dashboard, or see Cloudflare's
[object lifecycles](https://developers.cloudflare.com/r2/buckets/object-lifecycles/)
page for the API.

Objects expire one by one, by their own age. A session whose metadata was
refreshed later than its source (after a parser upgrade) can be left, for a
while, with metadata whose source is gone: `list` still shows it and `show
--normalized` reports the source missing, until the metadata expires too.

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
