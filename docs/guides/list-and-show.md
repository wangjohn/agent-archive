# Inspect the archive: list, show, feedback

`list` and `show` are read-only: they show what is in the bucket without
touching local collector state. `feedback` attaches your own assessment to
a session.

```sh
# Every archived session, newest first: ID, harness, capture time, origin,
# parser status, models, and skills used. Metadata only, never transcript text.
agent-archive list

# Narrow it down. --since takes a date, an RFC 3339 time, or an age.
agent-archive list --harness claude --model claude-opus-5 --since 7d
agent-archive list --imported          # only sessions backfill imported
agent-archive list --hook-captured     # only sessions captured as they ran
agent-archive list --skill review --skill-usage available
agent-archive list --skill review --skill-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
agent-archive list --complete          # complete parser coverage, no capture gaps

# For scripts: {"schema_version": 1, "sessions": [<metadata>...]}
agent-archive list --json

# One session's metadata sidecar, as JSON.
agent-archive show SESSION_ID
agent-archive show SESSION_ID --normalized   # also the verified conversation
```

The JSON documents are described in [JSON output](../reference/json-output.md).

`list` reuses unchanged metadata from a local cache (`--no-cache` to skip
it). A session whose metadata can't be read, for example because a newer
version wrote it, is left out with a warning on stderr; the rest are listed.

`show` prints conversation content only when asked: `--normalized` downloads
the session's source bundle, verifies its checksum and identity against the
metadata, and prints the normalized view (turns, tool calls, and
hook-reported final messages) after the sidecar. If the same session ID was
published under more than one harness, pass `--harness` to pick one. The
object layout is in
[bucket layout](../reference/bucket-layout.md).

Before setup has run, both commands print `Not set up.` to stderr and exit 1.

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
messages. `agent-archive show PARENT` adds `linked_session_availability`
(pending, unavailable, or unavailable-or-expired per child) beside the
sidecar's own fields, so `show` output is not itself an instance of
`metadata.schema.json`; validate stored metadata objects, not command output.
Select a child with `agent-archive show CHILD --normalized` for its verified
content. Links do not extend retention, and children are never downloaded
recursively. Codex and Cursor subagents are not captured yet.

## Skill evidence

During initial capture or new session activity, the background collector
checks known skill directories for the app. It records installed skills,
original instruction hashes, filtered instruction copies, and observation
times. These observations do not prove that the app discovered a skill, made
it eligible, or used that exact version in an earlier turn. Oversized,
truncated, nested, unreadable, and uninspected content is marked as a
coverage gap rather than failing the session. Hooks never scan the
filesystem.

`--skill-sha256` filters metadata only and distinguishes sessions using
different bytes under the same skill name. No parser version records both a
complete eligible-skill set and complete use observation, so non-use is never
proven: `--skill-usage eligible_no_use` is accepted, prints that explanation,
and exits 0 without listing anything. Sidecars from parsers before 0.4.0 (or
with a custom parser version) are never counted as observed non-use.

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
