# Git activity in session metadata: plan

> **Proposed.** Prepared 2026-09-30 from a read of the current code (no builds or tests were run). File and line references are to `main` at commit 7dfc1bb.

## Purpose

Today a session's `metadata.json` cannot answer "did this session commit, push, open a PR, or merge one, and when?". The closest signal is `tools_used`, which may show `mcp__github__create_pull_request 1` but not whether the call succeeded, which PR it made, or when. Shell work (`git commit`, `git push`, `gh pr create`, `gh pr merge`) is invisible: it is counted only as `Bash`.

The evidence is already in the archived source bundle. The privacy filter keeps tool arguments (the Bash `command`, MCP `owner`/`repo`/`pullNumber`), tool results joined by `tool_use_id` with `is_error`, and each record's `timestamp` and `gitBranch` (`internal/archive/adapters.go:430-486`). This plan derives a new metadata field from that evidence. **The filter does not change**, so there is no `FilterVersion` bump. Only the parser version is bumped, and the collector's existing parser-bump refresh republishes every retained session's metadata with the new field, history included.

## Decisions

- **Identifiers are published.** Each event may carry a commit SHA, a PR number, a repository (`owner/repo`), a branch, and a URL. Commit messages, PR titles and bodies, and command lines are **never** published.
- **Successes only.** An event is recorded only when the call's linked result is not an error **and** its output confirms the effect (a SHA, a `old..new branch -> branch` line, a PR URL, a merge confirmation). A call with no linked result, an `is_error` result, or output that does not match is not an event. Failed pushes, `nothing to commit`, rejected pushes, `--dry-run`, and auth failures therefore never appear.
- **What the agent did, not what happened to the repo.** A PR merged in the GitHub UI, or pushed from another terminal, is not observed. The field describes the session's own actions.

## Schema

Two optional additions to `schemas/metadata.schema.json`. Both are optional, so `MetadataSchemaVersion` stays 1.

### `git_activity`

An array, in transcript order, of at most `MaxGitActivity` (100) events:

```json
"git_activity": [
  {"kind": "commit",     "at": "2026-09-29T10:31:04Z", "source": "shell", "branch": "fix-oauth", "sha": "3f9c2ab"},
  {"kind": "push",       "at": "2026-09-29T10:31:09Z", "source": "shell", "branch": "fix-oauth", "repository": "wangjohn/agent-archive", "sha": "3f9c2ab", "url": "https://github.com/wangjohn/agent-archive/tree/fix-oauth"},
  {"kind": "pr_created", "at": "2026-09-29T10:31:40Z", "source": "mcp",   "branch": "fix-oauth", "repository": "wangjohn/agent-archive", "pr_number": 155, "url": "https://github.com/wangjohn/agent-archive/pull/155"},
  {"kind": "pr_merged",  "at": "2026-09-29T10:58:12Z", "source": "mcp",   "repository": "wangjohn/agent-archive", "pr_number": 155, "sha": "9e01d4c7…", "url": "https://github.com/wangjohn/agent-archive/pull/155"}
]
```

| Field | Type | Rule |
| --- | --- | --- |
| `kind` | enum `commit`, `push`, `pr_created`, `pr_merged` | Required. Readers accept any other lowercase kind a newer writer adds, as `gap_code` does. |
| `at` | date-time | The timestamp of the **result** record (when the effect was confirmed). Absent when the harness wrote none (Cursor text transcripts). |
| `source` | enum `shell`, `mcp` | Which recognizer produced the event. |
| `sha` | `^[0-9a-f]{7,40}$` | Commit: the short SHA `git commit` printed. Push: the new tip from `old..new`; absent for `[new branch]`, whose output has no SHA. Merge: the merge commit SHA when the MCP result or `gh` output reports one. |
| `branch` | string, 1-255 chars | Commit: from `[branch sha]`. Push: the remote ref from `-> ref`. PR: the head branch when known, otherwise the record's `gitBranch`. |
| `repository` | `^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$` | Parsed from the push remote, the PR URL, or the MCP `owner`/`repo` arguments. |
| `pr_number` | integer ≥ 1 | PR kinds only. |
| `url` | `format: uri`, `^https://` | **Rebuilt** from parsed parts (`https://<host>/<owner>/<repo>/pull/<n>`, or `/tree/<branch>` for a push), never copied. So it can carry no userinfo, query, fragment, or any text the parser did not validate. |

`additionalProperties: false` on the item. Every string field passes the same publishing step as `metadataToolName` (`internal/archive/metadata.go`): it is redacted again (a no-op on filtered text), has control characters stripped, and is capped.

### `counts` additions

`commits`, `pushes`, `prs_created`, `prs_merged`: uncapped integers, present whenever `counts.tool_calls` is (structured JSONL was parsed). Absent means unknown, never zero, as for the other counts. They stay exact when `git_activity` hits its cap, and they give `list` a cheap filter key.

## Recognizers

A new file, `internal/archive/git_activity.go`, with `deriveGitActivity(bundle SourceBundle, calls []NormalizedToolCall) ([]GitEvent, GitCounts)`. It walks `view.ToolCalls` in order. For each call with `IsError != nil && !*IsError` and a linked result, it dispatches on `callToolName(call)`:

### Shell calls (`shellToolNames`, `handoff.go:609`)

These are Claude `Bash`, Codex `shell`/`exec_command`/`local_shell_call`, and Cursor `run_terminal_cmd`. The command comes from `argumentText(input, "command", "cmd")` as `toolSummary` reads it. Codex's argv arrays are joined. The command is used only to **gate**: it must contain the relevant subcommand (`git commit`, `git push`, `gh pr create`, `gh pr merge`, allowing `git -C dir …` and chains with `&&`, `;`, and newlines). Evidence comes from the **output** (`call.resultText`), so one chained call (`git commit … && git push`) can yield several events.

| Kind | Command gate | Output evidence |
| --- | --- | --- |
| commit | `git commit` (also `--amend`) | `^\[(?P<branch>[^\s\]]+)(?: \(root-commit\))? (?P<sha>[0-9a-f]{7,40})\]` at line start |
| push | `git push`, excluding `-n`/`--dry-run` | A `To <remote>` line, then `^\s+[+ ]?(?P<old>[0-9a-f]{7,40})\.\.\.?(?P<new>[0-9a-f]{7,40})\s+\S+ -> (?P<ref>\S+)` or `^\s*\* \[new branch\]\s+\S+ -> (?P<ref>\S+)`. One event per updated ref. Lines with `! [rejected]` or `[up to date]` are not events. |
| pr_created | `gh pr create` | A line that is exactly `https://<host>/<owner>/<repo>/pull/<n>` |
| pr_merged | `gh pr merge`, excluding `--auto` (which only enables auto-merge) | `Merged pull request (?:<owner>/<repo>)?#<n>` or `Squashed and merged …` or `Rebased and merged …` |

Codex output may be a JSON wrapper (`{"output": …, "metadata": {"exit_code": N}}`) or carry an `Exit code N` line. The recognizer reads `output` from the wrapper, and treats a non-zero exit code as an error even when no `is_error` flag was recorded.

The remote in `To <remote>` may be `git@host:owner/repo.git`, `https://[redacted@]host/owner/repo(.git)`, `ssh://…`, or a local path. Only the host and `owner/repo` are kept. A local path or an unparseable remote yields a push event with no `repository`/`url`.

### MCP calls

GitHub MCP tool names are matched by suffix, whatever the server prefix (`mcp__github__…`, `mcp__<alias>__…`):

| Tool suffix | Kind | Evidence |
| --- | --- | --- |
| `create_pull_request` | pr_created | The result JSON's `number` and `html_url`/`url`. Falls back to a `/pull/<n>` URL found in the text. |
| `merge_pull_request` | pr_merged | The result JSON's `merged: true` (and `sha`). `pullNumber`, `owner`, and `repo` come from the call's input. |
| `push_files`, `create_or_update_file` | commit | The result's commit `sha`. These tools commit on the server, so the commit is the effect. The branch comes from the input's `branch`. |

`enable_pr_auto_merge` is not a merge and is ignored.

### Timestamps

`at` is `parseNativeTimestamp(bundle.NativeRecords[*call.ResultRecordIndex])`, the helper `views.go` already uses for `LatestRecordAt`. If the result record has none, `at` is absent.

## Wiring

1. `internal/archive/types.go`: add `GitEvent` and `Metadata.GitActivity []GitEvent \`json:"git_activity,omitempty"\``, and the four `*int` counts on `Counts`, with doc comments in the style of `FilesTouched`/`ToolsUsed`.
2. `internal/archive/metadata.go`: in `assembleParsedMetadata`, inside the existing `len(bundle.NativeText) == 0` branch beside `deriveToolsUsed`, set `metadata.GitActivity` and the counts. Text-only transcripts leave both unknown, as they do for `tools_used`.
3. `internal/archive/adapters.go:67`: bump `DefaultParserVersion`. The [portable handoff plan](portable-handoff-and-onboarding.md) also claims `0.14.0` for `repo_key`, so whichever lands second takes `0.15.0`. Update `dev/maintainers/versions.md`.
4. `schemas/metadata.schema.json`: add `git_activity` (with a `$defs/git_event`) and the four counts. Each description starts "From parser 0.14.0:", as `files_touched` does.
5. `internal/cli/show_summary.go`: add a `Git` row after `Tools`, for example `2 commits · 1 push · PR #155 opened · PR #155 merged`, wrapped with `wrapList`. It is left out when there is no activity, as other rows are.
6. `list --json` gains the field automatically, since each item is the sidecar. A `list --git pr_merged` style filter is a follow-up, not part of this change.
7. `internal/listingindex`: no change. Hints stay pointers, and the sidecar remains the authority.

## Tests

- `internal/archive/git_activity_test.go`: table-driven, one row per recognizer and harness (Claude Bash, Codex `exec_command` with its output wrapper, Cursor `run_terminal_cmd`, GitHub MCP). It covers:
  - Negatives: `is_error`, `nothing to commit`, `! [rejected]`, `Everything up-to-date`, `--dry-run`, `gh pr merge --auto`, and a call with no linked result.
  - Chained commands that yield several events.
  - A `[new branch]` push with no SHA.
  - A remote carrying redacted userinfo, where the rebuilt URL must not contain it.
  - A command that only mentions `git push` inside an `echo`, where the gate passes but no output evidence matches, so there is no event.
- Cap behavior: 150 commits yield 100 events and `counts.commits == 150`.
- Extend `schema_test.go` fixtures so the new fields validate, and extend `parser_fuzz_test.go` to run `deriveGitActivity` on fuzzed results (no panics, and every emitted string matches its schema pattern).
- `internal/cli/show_summary_test.go`: the new row, and no row when there is no activity.
- Fixtures use invented repos and SHAs only (see the PR template's privacy line).

## Docs

- `docs/reference/json-output.md` and `docs/reference/schemas.md`: the new fields.
- `docs/guides/list-and-show.md`: the `Git` row in the sample summary.
- `docs/security/privacy.md`: metadata now publishes repository names, branch names, commit SHAs, and PR numbers and URLs for sessions that pushed or opened PRs. It never publishes commit messages or PR text.
- `CHANGELOG.md`: an entry under Unreleased.

## Limits and follow-ups

- **Truncated output.** A result over the filter's 64 KB cap may lose its confirming line. The event is then dropped, which errs toward "not recorded" rather than a false positive.
- **Subagents.** Git work done in a subagent lands in the child session's metadata, not the parent's. Rolling it up is a follow-up.
- **Claude Code PR-link records.** Newer Claude Code builds may write a dedicated record when a PR is created. The Claude adapter keeps only `user`, `assistant`, `tool_use`, `tool_result`, `message`, and `summary` records (`adapters.go:115`), so such a record would be dropped as `unknown_record_type`. Admitting it is a filter change (a `FilterVersion` bump, a changelog section, and goldens). It is deferred until the record's shape is confirmed from a real transcript. It would add a third `source` value and no schema change.
- **Hosts.** Any host is accepted when it parses as `owner/repo` (GitHub Enterprise, GitLab-style URLs from `git push`). `pr_created` and `pr_merged` recognize only GitHub's `/pull/<n>` and `gh`/GitHub MCP.

## Work: one PR, about 500 lines plus tests and docs

Steps 1-5 above ship together, because the schema is `additionalProperties: false` and must change in the same PR as the writer. Implementation order: recognizers with unit tests, then wiring and schema, then the show row, then docs.
