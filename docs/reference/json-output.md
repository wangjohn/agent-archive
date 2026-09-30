# JSON output

These commands print JSON for scripts. Each document carries a
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

From parser `0.14.0` a sidecar also carries, all optional (absent means
unknown, never zero):

- `counts.reasoning_tokens`: tokens spent reasoning (Claude Code's
  `thinking_tokens`, Codex's `reasoning_output_tokens`). They are part of
  `output_tokens`, not in addition to it.
- `counts.tool_errors`: how many of `counts.tool_results` the app flagged as
  errors. Known for Claude Code and Cursor; absent for Codex, which does not
  flag them.
- `model_tokens`: the token counts split by model, as `{"model",
  "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
  "reasoning_tokens"}`, sorted by model. Each token field of `counts` is the
  sum of that field over `model_tokens`. Tokens on a record that names no
  model are under `unknown`. For Codex the model is the one its latest turn
  set, so a model switched mid-session splits there. At most 32 entries are
  kept, with a model id cut at 128 characters; beyond that the models with
  the fewest tokens are added together under `other`.
- `mcp_calls`: the (up to) 50 MCP servers the session called, as `{"name",
  "count"}`, by count, then name; the server is the part of an
  `mcp__<server>__<tool>` tool name. Codex's MCP calls do not name their
  server in what is retained, so they are not counted.

Token counts keep each app's own meaning: Claude Code's `input_tokens` leaves
out what was read from or written to the prompt cache, while Codex's
`input_tokens` includes its cached input and its cache-write input, which
`cache_read_tokens` and `cache_write_tokens` repeat (OpenAI reports both as
parts of the input, and a Codex record's `total_tokens` is input plus output).
`cache_write_tokens` is Claude Code's cache creation, or Codex's
`cache_write_input_tokens`. `stats` adds the two apps up in one meaning: a
Codex record's fresh input is its input minus both, and never below zero, so
a record whose cache counts exceed its input keeps both counts and has no
fresh input rather than a negative one.

From parser `0.15.0` a sidecar also carries the session's git work, also
optional:

- `git_activity`: up to 100 events, in transcript order, as `{"kind",
  "at", "source", "sha", "branch", "repository", "pr_number", "url"}`.
  `kind` is `commit`, `push`, `pr_created`, or `pr_merged`; `source` is
  `shell` (a `git` or `gh` command's output) or `mcp` (a GitHub MCP tool's
  result); `at` is when the confirming result was recorded. An event is
  recorded only when the call's result was not an error and shows the
  effect: the new commit's SHA, a push's ref update, the new pull request's
  URL, or a merge confirmation. A failed, rejected, up-to-date, or dry-run
  attempt, and `gh pr merge --auto`, are not events. Every other field is
  optional: `sha` is abbreviated as `git commit` printed it and absent for a
  new branch's push; `url` is rebuilt from the parsed host and repository
  and absent when the host is unknown (a `gh pr merge` whose command names no
  host, unless the session created that pull request) or is not a public DNS
  name (a local git proxy).
  Commit messages, pull request text, and commands are never kept.
- `counts.commits`, `counts.pushes`, `counts.prs_created`,
  `counts.prs_merged`: the same events counted, exact even beyond the 100
  listed.

Work done outside the session, such as a pull request merged on GitHub's
website, is not seen. Work a subagent did is in the subagent's own
metadata.

`show --transcript --json` prints a second JSON document after the sidecar:
the verified conversation as `turns`, `tool_calls`, `tool_results`, and
`hook_finals`. `show --normalized` is a deprecated name for it; its output
is unchanged, and it prints a deprecation note on stderr.

Both documents together are limited to `--max-bytes` (default 120000; `0`
for no limit). When that trims the normalized view, it gains a last field,
`trimmed`: `{"max_bytes": N, "omitted": [{"kind", "count"}, ...],
"full_record": PATH}`. The kinds, in the order they are applied, are
`tool_results` and `hook_finals` (entries dropped), `tool_input` (calls
whose `input` was dropped), `assistant_text` and `prompt_text` (turns whose
`text` was cut, each with `"text_truncated": true`), and `oldest_records`
(turns, calls, and results dropped, oldest first). `full_record` is the saved untrimmed output,
kept 7 days, and is absent if it could not be saved. Neither field exists in
untrimmed output. See [list and show](../guides/list-and-show.md#size).

## `stats --json`

```json
{
  "schema_version": 1,
  "generated_at": "2026-09-29T12:00:00-07:00",
  "filters": { "harness": "claude" },
  "window": { "days": 30, "timezone": "America/Los_Angeles", "first_day": "2026-08-31", "last_day": "2026-09-29", "...": "from, to, previous_from, previous_to" },
  "prices": { "version": "2026-09.2", "as_of": "2026-09-29", "currency": "USD", "...": "sources, notes, overridden" },
  "coverage": { "sessions": 412, "sessions_with_tokens": 371, "unknown_tokens_by_agent": { "cursor": 41 }, "...": "" },
  "daily": [ { "date": "2026-08-31", "sessions": 3, "tokens": 1200000, "cost": { "usd": 41.2, "partial": false, "unpriced_tokens": 0, "approximate": false } } ],
  "peak": { "date": "2026-09-17", "tokens": 4900000 },
  "peak_spend": { "date": "2026-09-27", "usd": 2910.4 },
  "overview": { "sessions": { "value": 412, "previous": 349, "change_pct": 18.05 }, "cache_share": 0.97, "...": "prompts, tokens, cost, active_days, streaks" },
  "agents": [], "models": [], "projects": [], "total_projects": 12,
  "composition": {}, "subagents": {},
  "skills": [], "display_skills": [], "total_skills": 9, "total_display_skills": 8, "mcp": {},
  "highlights": {},
  "heads_up": [ { "kind": "subagent_share", "share": 0.77, "tokens": 7700000000, "runs": 497 } ],
  "groups": { "by": "project", "rows": [] }
}
```

(`stats --html` writes a web page for people; it is not a format to parse.
Use `--json` in scripts.)

`stats` prints how you use your agents over a window of calendar days ending
today, with the period of the same length before it beside it, from session
metadata only. The document is the statistics engine's result with its fields
at the top level. Read the rules below before using a number:

- **Unknown is `null`, never `0`.** A number that no session in scope
  reported is `null`: Cursor records no tokens, Codex records no tool-error
  flag, and a sidecar written before parser `0.14.0` has no per-model split,
  tool errors or MCP calls. `coverage` says how many sessions each rests on
  (`sessions_with_tokens`, `unknown_tokens_by_agent`,
  `sessions_before_parser_0_14_0`, `sessions_priced_at_main_model`,
  `sessions_without_tool_errors`). A day with no sessions has `tokens: 0`; a
  day whose sessions record no tokens has `tokens: null`.
- **The window.** `--days N` (default 30) or `--since`, which starts the
  window on the local day it names (a date is a local day here, unlike
  `list --since`, which reads a date as midnight UTC). Days are counted in
  `window.timezone`. Sessions are placed by `captured_at`, the time
  `list --since` uses, not when they started: an imported session appears
  on the day it was imported. `overview` compares with `window.previous_from`
  up to `window.previous_to`; `highlights.month_rank` ranks this month so far
  against the five before it, from sessions outside the window too.
- **Sessions and subagents.** A subagent session's tokens, cost, tool
  results, skills and MCP calls roll up into its parent, which is one
  session; `subagents` reports the share of tokens they used.
  `coverage.orphan_subagents` counts subagents whose parent is not in the
  data, counted as sessions of their own.
- **Token counts.** `composition` splits tokens four ways that add up to
  `total`: `cache_read`, `cache_write`, `fresh_input` (input that was not
  read from the cache; Codex's cached input is subtracted from its input)
  and `output`. `reasoning_of_output` is a subset of `output`, not an
  addition. Totals saturate at the largest 64-bit integer instead of
  wrapping. Per-agent `cache_hit_rate` is in the JSON only.
- **Cost is an estimate** at list price from the dated price table in
  `prices` (`--prices FILE` puts your own entries on top and sets
  `overridden`); it is not a bill. The field is named `usd` whatever the
  currency. `usd` is `null` when nothing could be priced; `partial` with
  `unpriced_tokens` says tokens of models the table does not list (including
  `unknown` and `other`) are left out; `approximate` says some sessions with
  no per-model split were priced at their main model.
- **Skills** are counted in sessions that used them, not calls. **MCP**
  servers are counted in calls, and `mcp.scope` says which agents that
  covers (Claude Code and Cursor; Codex MCP calls are not recorded).
- **Spend by day.** Each `daily` entry has a `cost` in the same shape as
  every other cost, priced as the overall cost is from that day's sessions
  and placed the way tokens are (a subagent's cost is on its parent's day).
  A day without sessions costs a known `0`; a day whose sessions record no
  tokens (Cursor), or whose tokens are all of models the price table lacks,
  has `usd: null`, and `partial` with `unpriced_tokens` says the second.
  The days' `usd` add up to `overview.cost.value` (to within floating-point
  rounding). `peak_spend` is the day
  with the highest priced cost (the earliest on a tie), left out when no day
  cost more than zero; `peak` is still the day with the most tokens.
- **`overview.cache_share`** is the part of all tokens that were cache reads
  (0 to 1), or `null` when no session reports token counts or none reports
  cache counts; when it is present it equals `composition.cache_read.share`.
- **Top lists.** `projects`, `skills` and `mcp.servers` keep the top few rows
  (5); `total_projects`, `total_skills` and `mcp.total_servers` say how many
  there are. `projects` is ranked by estimated cost before it is cut, so the
  top five are the five that cost the most, not the five with the most tokens
  (see below). `models` lists every model family. `display_skills` is `skills`
  for showing to a person: a plugin prefix is stripped from each name
  (`anthropic-skills:docs` is `docs`; only the first `:` counts) and skills
  that then share a name are one row, counted in the sessions that used any
  of them (a session that used both counts once); `total_display_skills` is
  its length before the cut. `skills` keeps the names as recorded.
- **Project order.** `projects` (and `groups.rows` with `--by project`) are
  ordered by `cost.usd`, the highest first, and only then cut to the top
  five, so a project left out never cost more than one listed. A project
  whose cost is partial (`cost.partial`) is ordered on the `usd` it has; a
  project with no priced cost (`usd` is `null`) comes after every project
  that has one. Projects of equal cost, and those with none, are ordered by
  `tokens` (highest first, `null` last), then `sessions` (highest first),
  then `name`, so the order does not depend on the order sessions were read
  in. Until this change both lists were ordered by tokens, which cache reads
  dominate, so a cheaper project with more cache reads could push a dearer one
  out of the top five; the fields and `schema_version` (1) are unchanged, only
  the order and which five are kept.
- **`heads_up`** is what deserves a second look, at most three notes in
  priority order, `[]` when nothing does. Each note is data only, with a
  `kind` that says which fields it has; the words are the reader's:

  | `kind` | Applies when | Fields |
  | --- | --- | --- |
  | `subagent_share` | subagents used 25% or more of the window's tokens | `share` (0 to 1), `tokens`, `runs` (subagent runs rolled into their parents, which are not sessions of their own) |
  | `costliest_session` | the window has more than one session, and the costliest cost at least 10% of the priced spend and more than 1 (in the price table's currency) | `cost` (as `highlights.costliest_session.cost`), `cost_share`, `project` (left out when the session has none), `subagents` (runs it had), `drivers` (as in `highlights.costliest_session`, but left out, not `[]`, when there are none) |
  | `unmetered_sessions` | any session reports no token counts | `sessions`, `by_agent` (`harness`, `label`, `sessions`; most sessions first) |
  | `low_cache_hit` | the window's cache-hit rate is under 60%, over at least 50,000 input-side tokens | `hit_rate`, `input_tokens` |

  When more than three apply, the first three in this order are kept. The
  session behind `costliest_session` is `highlights.costliest_session`, which
  holds its ID.
- **`highlights.tool_errors`** is the share of tool results the app flagged
  as errors (this includes calls the user rejected or interrupted), over the
  `sessions` that record it; there is no per-tool breakdown.
- **`groups`** is present with `--by day|week|month|project`: `by` and
  `rows`, each with `key` (a date, a week's Monday, `2026-09`, or a project
  name), `sessions`, `prompts`, `tokens` and `cost`. Rows are chronological,
  or by estimated cost for `project` (in the order of `projects`, above).
  `projects` keeps only the top few of `total_projects`.
- `filters` echoes `--harness`, `--model` and `--hook-captured`/`--imported`
  (as `origin`: `hook` or `imported`); a filter that was not given is
  absent. The document holds counts, model, project, skill and MCP server
  names, and one session ID (`highlights.costliest_session`, which `show`
  opens). It never holds prompts, transcript text or paths. An empty archive
  prints a document with zero sessions. Usage errors (exit 2) print no JSON.
  `--json` is never paged.

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
| `agent_skills_disabled` | `true` when the agent skills are turned off (`agent-archive setup --no-skills`); absent otherwise. Setup then installs and refreshes none, and `agent_skills` is empty unless a file of setup's is left over (a restored backup, an interrupted removal): status warns about it, `agent_skills_out_of_date` is absent, and `agent-archive setup` removes it. `agent-archive setup --skills` turns them back on. |
| `applications[]` session counts | Per app, what the text status's app line counts. Only sessions the configuration publishes now count (the app's own, in the current destination, admitted in an included project once it was included; see [session eligibility](session-eligibility.md)). `sessions`: sessions the app's hooks registered, subagents included; `subagent_sessions`: the subagents among them (so top-level sessions are `sessions` minus `subagent_sessions`); `imported_sessions`: top-level sessions `agent-archive backfill` imported for the app (the top-level `imported_sessions` also counts imports into an earlier destination or from a project no longer included); `uploading_sessions`: top-level sessions, captured or imported, with work not yet in the bucket (the pending definition `collector.pending_count` uses, less sessions whose transcript is a recorded capture gap and those counted in `waiting_for_transcript_sessions`); `waiting_for_transcript_sessions`: top-level sessions pending only because no transcript was ever written for them, such as a Cursor chat with transcripts turned off; `uploading`: those sessions, failing ones first, then the most recently started, each with `archive_session_id`, `project`, `started_at`, `state` (`uploading`; `first_upload` when never uploaded yet; `failing` when the last pass recorded an issue for it, named in `issue` with a `collector.session_issues` code), and `imported` when backfill imported it. |
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
