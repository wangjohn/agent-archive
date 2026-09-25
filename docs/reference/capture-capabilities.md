# Capture capability evidence

Agent Archive reports application presence, installed version, documented hook
capabilities, adapter fixture coverage, and actual read-back verification as
separate facts. A configured or installed hook is not evidence that capture
works for an installed application version.

- Codex hooks document `SessionStart.source`, `transcript_path`, lifecycle
  events, and that the transcript format is not a stable hook interface:
  <https://learn.chatgpt.com/docs/hooks>.
- Codex version discovery uses `codex --version` and the documented bundled
  executable path: <https://learn.chatgpt.com/docs/reference/troubleshooting>.
- Claude Code documents `SessionStart`, native JSONL transcripts, and
  `claude --version`: <https://code.claude.com/docs/en/hooks> and
  <https://code.claude.com/docs/en/claude-directory>. Its hooks carry no
  version, so the archive attributes a capture to the top-level `version`
  stamp on each JSONL record (retained through the field allowlist).
- Cursor documents `sessionStart`, `cursor_version`, `transcript_path`, and
  lifecycle hooks. Agent Archive reads the macOS bundle version rather than
  substituting the separately versioned Agent CLI: <https://cursor.com/docs/hooks>.

## Tested app versions

| App | Version | How it was checked |
| --- | --- | --- |
| Claude Code | 2.1.x | Adapter built and checked against transcripts from this version. |
| Codex CLI | 0.155 | Adapter built and checked against transcripts from this version. |
| Cursor (desktop) | 3.21.13 | Hook payloads observed live; on 2026-09-23 (main `2a7a8fb`) a new chat registered at its first prompt, was published and read back, and `status` reported `verified_by_capture` for 3.21.13. A resumed chat was declined. |

Support is reported per Mac: `status --json` says `verified_by_capture` for
an installed version only once a session from that version has been
published and read back on that Mac, and `unverified` until then. A version
not in this table isn't assumed to work or to fail.

## Cursor 3.21.13 observations

This is the one place these observations are recorded; the
[eligibility rules](session-eligibility.md#cursor) that depend on them link
here. Cursor fresh-start capture is `documented`, and on the desktop app
3.21.13 its shape was observed from Cursor's Hooks output:

- A new Agent chat fires no `sessionStart`: its first hook is
  `beforeSubmitPrompt`, with `transcript_path` null, followed by
  `afterAgentResponse` and `stop`, which name
  `~/.cursor/projects/<workspace>/agent-transcripts/<id>/<id>.jsonl`.
- A resumed chat's first `beforeSubmitPrompt` already names its existing,
  non-empty transcript, and `sessionEnd` can fire mid-turn.
- Every payload carries `conversation_id`, `session_id` (the same value),
  `cursor_version`, `workspace_roots`, `composer_mode`, `model`,
  `model_id`, and `model_params`. Hook runs took 10–24 ms.
- Live read-back: on 2026-09-23 a new 3.21.13 chat was published a minute
  after its first prompt, its counts matched the transcript, read-back
  verified it, and `status` reported `verified_by_capture` for 3.21.13.

Not observed on 3.21.13, and kept fail-closed:

- `sessionStart`. The rule for it is kept for a version that fires one.
- `subagentStart` and `subagentStop`.
- A version where `session_id` and `conversation_id` differ. The transcript
  path is adopted only when its file name is the registered `session_id`,
  so such a version would never have its transcript adopted, and the chat
  would wait.
- Whether the transcript already holds the turn's records when
  `afterAgentResponse` names it. The collector re-reads on every pass, so a
  late write is picked up by the next one.

`cursor_version` is not evidence either way. For a Codex or Claude
`SessionStart` with no `source`, an empty or absent transcript file is still
the fallback proof, and a payload that names no transcript still proves
nothing.

## Capability states and subagents

`documented` means the vendor exposes the named evidence. `unavailable` means
the payload is insufficient for the archive claim. Installed-version support
remains `unverified` until a session from that observed version is published
and read back under the current configuration.

Claude child capture is `fixture_validated`: documented `SubagentStop` provides
`session_id`, `agent_id`, and `agent_transcript_path`, and synthetic JSONL fixtures
exercise the adapter. This does not certify any installed app version. A child
file must expose matching native parent and agent IDs and complete timestamp
provenance. Unknown formats are rejected. Codex and Cursor child capture remain
unavailable until their native identity formats are proven.

`SubagentStart` is deliberately not a freshness signal: Claude also fires it
when resuming or messaging an existing agent. A stop stages a local candidate;
background collection establishes native eligibility. Nothing reads the child
transcript or contacts storage in the hook.

"Complete timestamp provenance" applies to conversation-bearing records only.
Claude Code interleaves bookkeeping entries with no top-level timestamp —
`summary` and `file-history-snapshot` are the observed cases — and those do not
count against provenance; a `user`, `assistant`, `system`, or tool record
without a timestamp still rejects the child. A parent transcript that inlines a
subagent's records marks them `isSidechain`, and the parent's normalized view
excludes them from its own message, turn, and tool counts.

## User records the harness wrote

Claude Code marks who produced a user record. Filter 6 keeps `origin.kind`
and `promptSource`, and a record whose `origin.kind` is a known harness kind
(`task-notification`, a background-task notification) is not counted as a
prompt. Any other kind, including one a later version adds, stays a prompt. The
Claude desktop app also submits "The app was quit while you were working.
Please continue from where you left off" after a restart. That record has
`promptSource: "sdk"` and no `origin`, the same as a prompt submitted through
the SDK, so nothing in it distinguishes it from something a person sent and it
is still counted as a prompt. Matching its text is deliberately not done.

## Installed version versus captured version

`installed_version` comes from setup-time discovery and is labelled by
`installed_version_kind`: `cli` for a `--version` answer (Codex, Claude Code)
or `app_bundle` for a macOS bundle's `CFBundleShortVersionString` (Cursor).
Captured sessions report the harness's own version instead: Codex
`session_meta.cli_version`, Claude Code's per-record `version`, and Cursor's
hook `cursor_version`; these appear in `observed_harness_versions`. Support is
`verified_by_capture` only when a published, read-back session reports the
installed version.

When support is `unverified`, `installed_version_support_reason` says why:

- `no_verified_capture`: no published session has been read back with a
  harness version yet (including Claude Code sessions archived before record
  versions were retained).
- `no_matching_verified_version`: verified sessions came from other versions.
- `version_source_mismatch`: the installed version and every verified version
  follow different numbering schemes, so they cannot be compared. Cursor is the
  known case: the app bundle version and the hook's `cursor_version` are not
  guaranteed to share a scheme, and this has not been verified against a live
  installation. A mismatch here is a limitation of the comparison, not evidence
  that capture fails.

`application-versions.json` is advisory. If it cannot be read, `status` reports
a warning, treats installed versions as `unknown`, and continues.
