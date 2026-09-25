# JSON output

Three commands print JSON for scripts. Each is a stable contract: fields may
be added, but an existing field keeps its meaning, and an incompatible change
bumps the document's `schema_version`. None of them ever contains
conversation content, except `show --normalized`, which you ask for
explicitly.

## `list --json`

```json
{
  "schema_version": 1,
  "sessions": [ { "...": "one metadata sidecar per matching session" } ]
}
```

- `sessions` holds each matching session's metadata sidecar exactly as
  stored, newest capture first, filtered by the same flags as the text
  listing (`--harness`, `--model`, `--since`, `--skill`, `--complete`,
  `--imported`, …). Each item is an instance of
  [`metadata.schema.json`](schemas.md). An empty result is `[]`, never
  `null`.
- `unavailable` is present only when the query can't return sessions yet
  (today: `--skill-usage eligible_no_use`); it holds the explanation the text
  listing prints, and `sessions` is empty.
- A sidecar that can't be read (deleted mid-listing, or written by a newer
  version) is left out; a warning naming it goes to stderr, never stdout.

## `show`

`show SESSION_ID` always prints JSON (`--json` is accepted for consistency).
It is the session's metadata sidecar with one extra field,
`linked_session_availability`, resolved live for each linked subagent
session. Because of that field, `show` output is a view of a metadata object,
not an instance of `metadata.schema.json`; validate stored sidecars (or
`list --json` items), not `show` output.

`show --normalized` prints a second JSON document after the sidecar: the
verified conversation as `turns`, `tool_calls`, `tool_results`, and
`hook_finals`.

## `status --json`

Top-level fields (versioned by `schema_version`, currently `3`):

A time that is not known yet is left out rather than printed as a zero
time: a missing time field means "never". This applies to
`storage_verified_at`, `storage_access_confirmed_at`,
`authentication.checked_at`, `collector.last_scan_at`,
`collector.last_published_at`, and, per application and project,
`verified_at`, `last_published_at`, and `installed_version_observed_at`.
Treat an absent field and `null` the same way.

| Field | Meaning |
| --- | --- |
| `state`, `code`, `next_action` | Overall state as shown in the text output (`Ready`, `Needs attention`, …), a stable code for it (`ready`, `needs_attention`, `awaiting_capture`, `paused`, `not_configured`, `recovery_required`, …; codes never change with wording), and the suggested next step. |
| `storage` | The destination, as `provider / bucket / prefix`. |
| `configuration_id` | The identifier of the saved configuration this status describes; collector records made under another configuration are ignored. |
| `storage_verified_at` | When **setup's** storage check (write, read, list, delete of a probe object) last passed for this configuration. Setup only. |
| `storage_access_confirmed_at`, `storage_access_confirmed_by` | The latest confirmation that the destination is reachable with the configured credentials, and by whom: `setup`, or `collector` (its access probe, or a pass that uploaded). Use this to tell whether capture can still reach the bucket. |
| `authentication` | The last storage health check: state (`verified`, `stale_configuration`, …), time, and whether it came from a manual `sync` or the background collector. |
| `privacy`, `privacy_evidence` | Bucket privacy: `verified_private`, `public_or_risky`, or `not_verified`, with the reason, scope, and check time. |
| `background` | The launchd job: `loaded`, `running`, `missing`, `another_installation` (launchd runs this installation's label from a different plist, which is left alone), `broken` (the job runs an executable that no longer exists), or `unknown`. |
| `paused` | Whether collection is paused. |
| `projects` | Included project roots. |
| `applications[]` | Per app: hook state (`installed`, `missing or incomplete`, `broken`), installed version and its support (`verified_by_capture` once a session from that version was read back, else `unverified`), capture evidence (`configured`, `hook_observed`, `captured_locally`, `published`, `read_back_verified`, with counts), `sessions_with_capture_gaps`, observed app and adapter versions, and per-project breakdowns. |
| `collector` | The last pass: `last_scan_at`, `last_published_at`, `pending_count`, `last_error`, `session_issues` (per session), `quarantined_files` (state files moved aside; see [local state](local-state.md)), and `unrefreshable_summaries` (sessions whose metadata this version can't refresh). |
| `capture_diagnostics` | Content-free records of sessions a hook declined, for included projects. |
| `imported_sessions`, `imported_pending`, `imported_with_issues`, `last_import` | Backfill imports. |
| `warnings` | Local files status couldn't read; everything else is still reported. |

Before setup, `state` says setup is needed, `background` is `missing`, and
`authentication` is `not_configured`. When a command has held the collector
lock for over two hours, `next_action` says collection is stuck and names the
command and process ID.

## `backfill --dry-run --json` and `handoff --format json`

`backfill --dry-run --json` prints the import plan (projects, counts per
app, skip reasons, retention date). It holds no transcript paths, session
IDs, or content, but project folders are absolute paths: `projects[].root`,
`projects[].kept_out`, and `filters.projects`. `storage_checked` is always
`false`, because a dry run writes nothing and the storage check writes a test
object; only an import checks storage, before it asks.
`handoff --format json` prints the handoff document. Both follow the same
add-only rule but are not yet versioned documents; prefer the text output for
anything a person reads.
