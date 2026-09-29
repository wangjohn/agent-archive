# Inspect the archive: list, show, feedback

`list` and `show` are read-only: they show what is in the bucket without
touching local collector state. `feedback` attaches your own assessment to
a session.

```sh
# Newest archived sessions matching the filters (at most 50 by default):
# title (first filtered prompt preview), relative time, harness, project,
# and short ID. Metadata only, never full transcript text. On an interactive
# terminal, pick a numbered row to see that session's summary (see below).
# Otherwise the table is paged through $PAGER (or less; see Scrolling below);
# use --no-pager to print directly.
agent-archive list
agent-archive list --limit 0          # every match, not just the newest 50
agent-archive list --limit 200
agent-archive list --verbose          # full IDs, absolute times, origin, parser

# Narrow it down. --since takes a date, an RFC 3339 time, or an age.
agent-archive list --harness claude --model claude-opus-5 --since 7d
agent-archive list --imported          # only sessions backfill imported
agent-archive list --hook-captured     # only sessions captured as they ran
agent-archive list --skill review --skill-usage available
agent-archive list --skill review --skill-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
agent-archive list --complete          # complete parser coverage, no capture gaps

# For scripts: {"schema_version": 4, "sessions": [...], "limit", "returned",
# "total_matched_known"}. "total_matched" is omitted when the indexed
# listing stops after the limit. Never paged
# or interactive.
agent-archive list --json
agent-archive list --json --limit 0
agent-archive list --rebuild-index  # one-time full scan for older archives

# One session's summary: title, when, app, models, activity, skills,
# subagents, and capture gaps. With no SESSION_ID on a terminal, the same
# session browser as list. A title substring or short ID also works.
agent-archive show
agent-archive show SESSION_ID
agent-archive show "OAuth callback"
agent-archive show "OAuth callback" --harness codex  # search one app
agent-archive show SESSION_ID --json         # the metadata sidecar, for scripts

# The conversation itself, only when you ask for it.
agent-archive show SESSION_ID --transcript          # prompts, replies, tool calls
agent-archive show SESSION_ID --transcript --full   # also trimmed tool results
agent-archive show SESSION_ID --transcript --json   # the normalized view as JSON
```

A summary looks like this:

```text
Fix flaky OAuth callback tests
claude · agent-archive · 2h ago                                      ✓ completed

  When      Sep 29, 10:14 → 11:02 (48m span)
  Agent     Claude Code 2.4.1
  Model     claude-opus-5-5 (high reasoning) · 31 responses
            claude-haiku-4-5 · 4 responses
  Activity  35 turns · 212 messages · 148 tool calls · 3 shell commands ·
            1 compaction
  Skills    code-review, simplify
  Subagents 2 linked (1 available, 1 expired)

  ID 03e60c25f1a04b7c9d2e8f6a1b3c5d7e
     origin hook · parser 0.12.0 (partial) · filter 12

  Transcript: agent-archive show 03e60c25f1a04b7c9d2e8f6a1b3c5d7e --harness claude --transcript
  JSON:       agent-archive show 03e60c25f1a04b7c9d2e8f6a1b3c5d7e --harness claude --json
```

Rows the metadata has no data for are left out; a count that is unknown is
not shown as zero, and shell commands and compactions are listed only when
there were some. A model's count is how many responses it gave. Times are in your local time zone. The metadata records
when a session started and when it was last captured, not when it ended, so
the time between them is labelled a span. A session with capture gaps lists
each gap's code once, with how often it occurs.

## Browsing on a terminal

When stdin and stdout are both terminals, `list` and bare `show` open a
session browser on the terminal's alternate screen, so the list and a
session's summary replace each other instead of piling up:

- Enter a row number or short SESSION_ID to see that session's summary.
- In the summary, `t` opens its transcript through the pager (quit the pager
  to come back), Enter or `b` returns to the list, and `q` quits. `less`
  keeps even a one-screen transcript open until you press `q`; after another
  pager, press Enter to return to the summary.
- `q` (or an empty answer at the list, or Ctrl-D) quits from anywhere. The
  last summary you viewed is printed to the normal screen as the browser
  closes, so its ID stays in your scrollback.

Bare `show --transcript` is a usage error: pick a session with `show` and
press `t`, or give a SESSION_ID. Bare `show --json` keeps a one-shot picker
and prints the chosen sidecar.

On an interactive terminal, bare `agent-archive` (no command) opens the
same session browser as `list` when capture is already set up.

The JSON documents are described in [JSON output](../reference/json-output.md).

`list` reuses unchanged metadata from a local cache (`--no-cache` to skip
it). A session whose metadata can't be read, for example because a newer
version wrote it, is left out with a warning on stderr; the rest are listed.

`list` shows each session's `title` when metadata has one (a short preview of
the first filtered human prompt, derived at publish time). Older sidecars
without a title fall back to the short SESSION_ID in that column. `project_name`
in metadata labels the project when present; otherwise `list` uses this Mac's
configured project basename. The ID column is a short prefix you can pass to
`show`; if multiple archived IDs share that prefix, use a longer ID from
`list --verbose` or add `--harness`. Projects with the same basename stay in
separate groups, labeled with their project ID prefixes.

`show` prints conversation content only when asked, with `--transcript` or
the browser's `t`: it downloads the session's source bundle, verifies its
checksum and identity against the metadata, and prints each prompt, the
agent's replies, one line per tool call (`▸ Bash go test ./...`, marked ✗
when the call failed), the `!` shell commands (`$ make test`) and local
slash commands (`» /model`, with the app's reply) you ran, compaction
summaries, notices the app posted (such as a background task finishing,
with the agent's reply under it), and any final response a hook reported
that the transcript lacks. Edit bodies are never shown. `--full` adds each
tool result and shell command's output, trimmed to its first and last
lines; it does not combine with `--json`, which has every retained result. On a terminal the summary and
the transcript are paged like `list` (see [Scrolling](#scrolling));
`--no-pager` prints them directly. `--transcript --json`
prints the sidecar and then the normalized view (turns, tool calls, tool
results, and hook-reported final messages) as JSON. `--normalized`, its
former name, still works and prints a deprecation note on stderr. If the
same session ID was published under more than one harness, pass `--harness`
to pick one. The object layout is in
[bucket layout](../reference/bucket-layout.md).

Before setup has run, both commands print `Not set up.` to stderr and exit 1.

## Scrolling

On a terminal, anything longer than a screen goes through a pager: the
`list` table outside the browser, `show SESSION_ID`'s summary, a transcript,
`status`, and `purge plan`. Piped or redirected output, and `--json`, are
never paged; `--no-pager` prints directly on a terminal too.

With no pager set, or with the pager set to a bare `less` (as oh-my-zsh
does), Agent Archive runs `less` its own way: it quits at once when the text
fits on one screen and otherwise shows the keys on its last line:

```text
lines 1-48 of 1210 - arrows/space scroll, / search, q quit
```

- The mouse wheel, arrow keys, space and `b` (page down and up), and `g` and
  `G` (top and bottom) scroll; `/` searches, `n` finds the next match; `q`
  quits. From the session browser, `q` goes back to the summary.
- With `less` 551 or later (macOS ships a newer one), the wheel scrolls the
  text, three lines at a time, and the text stays on the screen after `q`.
  Because `less` then reads the mouse, hold Option while dragging to select
  text in iTerm2 (Shift in most other terminals; in Terminal, turn off View
  > Allow Mouse Reporting).
- `less` 530 to 550 shows the text on the terminal's alternate screen,
  where the terminal turns the wheel into arrow keys, and clears it on `q`.
- An older `less` keeps the text on the normal screen, where the wheel may
  scroll the terminal instead. A `less` that doesn't report its version
  (BusyBox's) runs as `less -FR`, without key hints.

To use another pager, set `AGENT_ARCHIVE_PAGER` (or `PAGER`), for example
`AGENT_ARCHIVE_PAGER='less -R'`. It runs as given, without the key hints or
mouse options, with two additions as git makes them: `LESS=FRX` when `LESS`
is not set, and `LV=-c` when `LV` is not set. From the session browser, a
plain `less` command also gets `-+F`, so a short transcript waits for `q`.
Set either variable to `cat`, or to nothing, to never page.

## Model and setting keys

Metadata model keys `gen_ai.provider.name`, `gen_ai.request.model`, and
`gen_ai.response.model` follow OpenTelemetry GenAI semantic conventions
v1.37.0 at commit `aec6e9d3e86754683dab7c707655d69d953b2768`.
`agent_archive.request.model_label`, `agent_archive.request.reasoning_level`,
and `agent_archive.request.setting.*` are archive-local extensions. The
archive is not an OTLP export. Metadata derived by parser 0.2.x may still
contain the former `gen_ai.request.reasoning.level`,
`gen_ai.request.model.label`, and `gen_ai.request.setting.*` keys;
regenerating metadata with a later parser moves them without changing the
source bundle.

## Linked subagent sessions

A Claude Code subagent (`SubagentStop`) becomes its own archived session with
`parent_session_id`, once the collector validates it against an already
accepted parent: matching parent and agent IDs, and a start after the
parent's start and the project's activation. The parent gets
`linked_sessions`; it does not embed the child's transcript or count its
messages. `agent-archive show PARENT` counts them on its Subagents row, and
`show PARENT --json` adds `linked_session_availability` (pending,
unavailable, or unavailable-or-expired per child) beside the sidecar's own
fields, so `show --json` output is not itself an instance of
`metadata.schema.json`; validate stored metadata objects, not command output.
A child whose transcript is still missing or empty 30 minutes after its
`SubagentStop` (Claude Code reports some background agents with a path it
never writes) is dropped, its parent's link becomes unavailable, and the
parent records a `subagent_transcript_never_written` capture gap.
A child's summary names its parent. Select a child with
`agent-archive show CHILD --transcript` for its verified content. Links do
not extend retention, and children are never downloaded
recursively. Codex and Cursor subagents are not captured yet.

## Skill evidence

During initial capture or new session activity, the background collector
checks known skill directories for the app. It records installed skills,
instruction hashes (SHA-256 of each SKILL.md with its credentials
redacted), filtered instruction copies, and observation
times. These observations do not prove that the app discovered a skill, made
it eligible, or used that exact version in an earlier turn. Oversized,
truncated, nested, unreadable, and uninspected content is marked as a
coverage gap rather than failing the session. Hooks never scan the
filesystem.

`--skill-sha256` filters metadata only and distinguishes sessions using
different bytes under the same skill name. No parser version records both a
complete eligible-skill set and complete use observation, so non-use is never
proven. `--skill-usage eligible_no_use` is unsupported and exits with a usage
error on stderr (code `2`) and no result on stdout. Use `used` or `available`
for supported skill queries. Sidecars from parsers before 0.4.0 (or with a
custom parser version) are never counted as observed non-use.

## Feedback

Write your assessment to a private UTF-8 text file, then attach it to a
session owned by this Mac:

```sh
agent-archive feedback SESSION_ID --file /private/path/feedback.txt
agent-archive sync
```

Feedback is filtered before it enters the local upload queue. It records user
provenance and observation time; finishing a turn is never treated as
success. The command rejects excluded sessions and sessions retired by a
destination change. Feedback for a paused, still-included session waits for
resume.
