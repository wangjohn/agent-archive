# JSON output

Three commands print JSON for scripts. Each document carries a
`schema_version`. The aim is that fields are only added, an existing field
keeps its meaning, and an incompatible change bumps `schema_version`, from
`v0.1.0` on. Check `schema_version`, and read the
[changelog](../../CHANGELOG.md) when you upgrade. None of them ever contains
conversation content, except `show --transcript --json`, which you ask for
explicitly.

## `list --json`

```json
{
  "schema_version": 4,
  "sessions": [ { "...": "one metadata sidecar per matching session" } ],
  "limit": 50,
  "returned": 50,
  "total_matched_known": false,
  "truncated": true
}
```

- `sessions` holds each matching session's metadata sidecar exactly as
  stored, newest capture first, filtered by the same flags as the text
  listing (`--harness`, `--model`, `--since`, `--skill`, `--complete`,
  `--imported`, `--limit`, …). Each item is an instance of
  [`metadata.schema.json`](schemas.md). An empty result is `[]`, never
  `null`.
- `limit` is the `--limit` value (`50` by default; `0` means no cap).
  `returned` is `sessions.length`. `total_matched_known` says whether the
  count is exact. When false, `total_matched` is omitted and `truncated` is
  true: the indexed read stopped once it found one more match than the limit.
  When true, `total_matched` is the exact match count and `truncated` is
  present only if the limit cut it short. Use `--limit 0` for an exact count.
- Unsupported filter values return exit code `2` with an explanation on
  stderr and no JSON on stdout. This includes `--skill-usage eligible_no_use`:
  current parsers cannot prove non-use. Schema version `3` removed the
  version `2` `unavailable` field; version `4` adds explicit count knowledge.
- A sidecar that can't be read (deleted mid-listing, or written by a newer
  version) is left out; a warning naming it goes to stderr, never stdout.
- `--json` is never auto-paged, even on a terminal.

## `show`

`show SESSION_ID --json` prints JSON; without `--json`, `show` prints a
readable summary. (Before this release `show` always printed JSON: a script
that parses `show` output must now pass `--json`.) It is the session's
metadata sidecar with one extra field,
`linked_session_availability`, resolved live for each linked subagent
session. Because of that field, `show` output is a view of a metadata object,
not an instance of `metadata.schema.json`; validate stored sidecars (or
`list --json` items), not `show` output.

From parser `0.13.0` a sidecar also summarizes what the session did, without
any transcript text or file path:

- `ended_at`: the latest timestamp any retained record carries, never
  earlier than `started_at`. Absent when no record has a timestamp (Cursor
  transcripts).
- `tools_used`: the (up to) 10 most-called tools as `{"name", "count"}`,
  by count, then name. Absent when no named tool call was seen or the counts
  are unknown; `counts.tool_calls` tells the two apart.
- `counts.files_touched`: how many distinct files the session's editing
  calls named, the same files `handoff` lists. Like every count, it is
  absent when unknown, and `0` means known none.

Sidecars written by an older parser gain these fields on the next metadata
refresh.

`show --transcript --json` prints a second JSON document after the sidecar:
the verified conversation as `turns`, `tool_calls`, `tool_results`, and
`hook_finals`. `show --normalized` is a deprecated name for it; its output
is unchanged, and it prints a deprecation note on stderr.

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
| `skill_evidence` | Effective filesystem skill evidence policy: `none`, `metadata`, or `body`. Older configs without the field report `body`. |
| `applications[]` | Per app: hook state (`installed`, `missing or incomplete`, `broken`, or `unknown` when the hook file could not be read or no executable is recorded to check the hooks against; `warnings` then names the file), `other_installations` (the data directories of other agent-archive installations whose hooks are in the same hook file; this installation never changes them, and setup won't install beside them), installed version and its support (`verified_by_capture` once a session from that version was read back, else `unverified`), capture evidence (`configured`, `hook_observed`, `captured_locally`, `published`, `read_back_verified`, with counts), `sessions_with_capture_gaps`, observed app and adapter versions, and per-project breakdowns. |
| `agent_skills` | The agent skill files (the `/handoff` skill) setup installed that are there now (absolute paths; absent when there are none). A file at one of those paths without setup's marker line is the person's own, and one naming another data directory is another installation's; neither is listed. |
| `agent_skills_out_of_date` | The files in `agent_skills` whose text differs from what this version of `agent-archive` writes (an earlier release wrote them, or the executable moved); `agent-archive setup` refreshes them, and status warns about each. Absent when there are none, or when no executable is recorded to compare with. |
| `collector` | The last pass: `last_scan_at`, `last_published_at`, `pending_count`, `last_errors` (each problem the pass recorded, one per entry; a status file from an older version may have only `last_error`), `last_error` (the same problems joined with `; `, kept for older readers), `session_issues` (per session, the kind of failure as a code: `storage_auth`, `storage_unavailable`, `local_state_unreadable`, `subagent_not_captured`, `retention_failed`, or `capture_failed`; a status file from an older version may have `capture_or_publication_failed`, which is `capture_failed`, `transcript_size_limit`, or `transcript_discontinuity`; accept codes not listed), `issue_counts` (how many sessions and subagents have each code, the counts the last error's summary of failed sessions is built from; absent when a failure before collection, such as storage that could not be opened, replaced that summary), `quarantined_files` (state files moved aside; see [local state](local-state.md)), `unrefreshable_summaries` (sessions whose metadata this version can't refresh), `waiting_subagents` (subagents whose transcripts weren't written yet; not a problem), `running_subagents` (subagents resumed after their last stop and still writing, kept at their last stop until they stop again or go quiet for 30 minutes; not a problem), and `expired_subagents` (the Claude Code subagents dropped in the last 7 days because their transcripts were never written, at most 100, oldest first, each with `archive_session_id`, `agent_type` when the hook reported a valid one, and `expired_at`; kept on this Mac only, never uploaded; not a problem). |
| `capture_diagnostics` | Content-free records of sessions a hook declined or deferred, for included projects. `hook_busy` means a hook timed out waiting for the capture lock; a proven first start may be replayed on the next collector pass. |
| `imported_sessions`, `imported_pending`, `imported_with_issues`, `last_import` | Backfill imports. |
| `warnings` | Problems status found but reported around: each local file it couldn't read (named, with what to do; everything else is still reported), a hook file it couldn't check, and another installation's hooks in this one's hook files. |

Before setup, `state` says setup is needed, `background` is `missing`, and
`authentication` is `not_configured`. When a command has held the collector
lock for over two hours, `next_action` says collection is stuck and names the
command and process ID.

## `backfill --dry-run --json` and `handoff --format json`

`backfill --dry-run --json` prints the import plan (projects, counts per
app, skip reasons, retention date). It holds no transcript paths, session
IDs, or content, but project folders are absolute paths: `projects[].root`,
`projects[].kept_out`, `projects[].kept_out_unchecked`, and `filters.projects`. `storage_checked` is always
`false`, because a dry run writes nothing and the storage check writes a test
object; only an import checks storage, before it asks.
`handoff --format json` prints the handoff document. Both follow the same
add-only rule but are not yet versioned documents; prefer the text output for
anything a person reads.
