# Continue a session in another agent (handoff)

`handoff` prints a session as a prompt another coding agent can pick up from:
where it left off, the latest plan, files touched, then every prompt with the
agent's replies, one-line tool-call summaries, trimmed tool output, and any
summary Claude Code wrote when the session was compacted. Edit bodies are left
out; the receiving agent should read the files as they are now.

```sh
# Continue in Claude Code what you started in Codex, in the same repository
claude "$(agent-archive handoff --latest --harness codex)"

# Continue in Codex what you started in Claude Code
codex "$(agent-archive handoff --latest --harness claude)"

# A specific session, from `list`, written to a file
agent-archive handoff SESSION_ID --output /tmp/handoff.md

# A transcript the archive never captured, on this Mac; needs no setup
agent-archive handoff --file ~/.codex/sessions/.../rollout-....jsonl --harness codex
```

## Where the session comes from

A session registered on this Mac is read from its transcript as it is now, so
a handoff right after you stop needs no sync and works while collection is
paused; nothing is uploaded. Otherwise the session is downloaded from the
archive, which is how a second Mac hands off a session from the first.
`--source local|archive` forces one or the other; with neither, a local
transcript that cannot be read falls back to the archive's copy.

`--latest` names its choice on stderr, passes over sessions with no prompt
yet, and, when run by an agent that names its own session (Claude Code does,
through `CLAUDE_CODE_SESSION_ID`), skips that session. It matches the current
directory's project, not projects beneath it. On another Mac it matches the
project only when the repository is checked out at the same path; when
nothing matches it lists the five most recent archived sessions with the
command for each. Uncommitted changes stay on the machine that made them, so
push a branch before continuing elsewhere.

## Size

Output is limited to 120,000 bytes (about 30k tokens; `--max-bytes`, `0` for
no limit). Over the limit, older tool output is dropped first, then older
tool calls are collapsed to counts, then older agent messages are shortened,
then long prompts are truncated. The last three exchanges (up to their last
twenty steps) and every prompt are kept. When anything is trimmed, the
untrimmed version is saved under the data directory in `handoffs/` (mode
0600, removed after 7 days and by `uninstall --delete-local-data`) and its
path is named at the end, so the receiving agent can read what was omitted.
`--file` without setup never creates the data directory, so a trimmed
`--file` handoff is not saved in full there; use `--max-bytes 0` to print all
of it.

Everything printed has passed the same [privacy filter](../security/privacy.md)
as the archive. Cursor transcripts record no tool results, so a Cursor
handoff says so and shows none. `--format json` prints the same content as
JSON. The format is specified in the [handoff design](../design/handoff.md).
