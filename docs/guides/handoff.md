# Continue a session in another agent (handoff)

`handoff` prints a session as a prompt another coding agent can pick up from:
where it left off, the latest plan, files touched, then every prompt with the
agent's replies, one-line tool-call summaries, trimmed tool output, and any
summary Claude Code wrote when the session was compacted. Edit bodies are left
out; the receiving agent should read the files as they are now.

```sh
# On an interactive terminal, pick an archived session by number or short ID
agent-archive handoff

# Launch a local Codex session with the latest local Claude Code session
agent-archive handoff --latest --harness claude --to codex

# Or launch Claude Code or Cursor Agent locally
agent-archive handoff SESSION_ID --to claude
agent-archive handoff SESSION_ID --to cursor

# Continue in Claude Code what you started in Codex, in the same repository
claude "$(agent-archive handoff --latest --harness codex)"

# Continue in Codex what you started in Claude Code
codex "$(agent-archive handoff --latest --harness claude)"

# A specific session, from `list`, written to a file
agent-archive handoff SESSION_ID --output /tmp/handoff.md

# A transcript the archive never captured, on this Mac; needs no setup
agent-archive handoff --file ~/.codex/sessions/.../rollout-....jsonl --harness codex
```

`--to` launches the installed local `claude`, `codex`, or `cursor-agent` CLI
in the current directory (or `--project DIR` with `--latest`). It writes the
filtered handoff to a private temporary file and sends the agent a short prompt
to read it; the file is removed when the agent exits. The session is read as
for any handoff: this Mac's transcript when there is one, else the archive's
copy. It never starts a remote agent. The receiving agent is told that Agent Archive exists, where to
find its executable, and how to ask for more context.
`agent-archive show SESSION_ID --transcript` reads the published archive copy,
which may lag the local transcript or not exist yet.
`agent-archive handoff SESSION_ID --source local --max-bytes 0` reads the
current, complete **filtered** local record. Neither command exposes
unfiltered raw transcript data. The handoff uses the usual 120,000-byte
default budget; `--max-bytes 0` includes all filtered content.

## From inside an agent: /handoff

Setup installs a `handoff` skill for the apps it sets up:
`~/.claude/skills/handoff/SKILL.md` (in `$CLAUDE_CONFIG_DIR` when set) for
Claude Code, and `~/.agents/skills/handoff/SKILL.md` for Codex and Cursor.
In Claude Code, `/handoff codex` runs `agent-archive handoff --to codex` for
the current session; in Codex, ask for `$handoff` (or pick it from `/skills`)
and name the agent. With no agent named, the skill picks another agent than
the one you are in. Each file carries a marker line: setup replaces and
uninstall removes only a file with it, so delete that line to keep an edited
copy. A file already at that path without it is left alone, and setup says
so. An installation with `AGENT_ARCHIVE_HOME` set runs the command with it,
and leaves the skill of another installation sharing the same home folder
alone. `agent-archive status --verbose` lists the installed files.

## Where the session comes from

With no session selector, `handoff` opens a numbered session picker when
stdin and stdout are terminals. It lists this Mac's sessions, including ones
marked `not yet uploaded`, together with archived ones, most recently active
first; a session that is both appears once. Sessions with no prompt yet are
left out, and `--harness` narrows the list. When the archive cannot be read
(offline, say), the picker lists this Mac's sessions and says why archived
ones are missing. Quit with `q` without producing a handoff. In a script or
pipeline, pass a session ID, `--latest`, or `--file`.
The picker never opens when a coding agent runs the command, even in a
pseudo-terminal: `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or
`CURSOR_AGENT` in the environment (or `AGENT_ARCHIVE_NONINTERACTIVE=1`) turns
prompts off, and `handoff` without a selector is a usage error (exit 2)
instead. `AGENT_ARCHIVE_NONINTERACTIVE=0` turns them back on; see
[configuration](../reference/configuration.md#environment-variables).

With `--to` and no selector, run from inside an agent, `handoff` hands off
the session it is running in without asking: the one Claude Code names in
`CLAUDE_CODE_SESSION_ID` or Codex in `CODEX_THREAD_ID`. Cursor names no
session, so inside Cursor (`CURSOR_AGENT` is set) it takes the newest Cursor
session for the current directory, as `--latest --harness cursor` would.
That is not a prompt, so it works while prompts are off. Otherwise a terminal
gets the picker, and anything else is told to name a session.

A session registered on this Mac is read from its transcript as it is now, so
a handoff right after you stop needs no sync and works while collection is
paused; nothing is uploaded. Otherwise the session is downloaded from the
archive, which is how a second Mac hands off a session from the first.
`--source local|archive` forces one or the other; with neither, a local
transcript that cannot be read falls back to the archive's copy.

`--latest` names its choice on stderr, passes over sessions with no prompt
yet, and, when run by an agent that names its own session (Claude Code does,
through `CLAUDE_CODE_SESSION_ID`), skips that session unless `--to` is used.
With `--to`, the calling session is eligible because it is the source being
handed off. `--latest` matches the current
directory's project, not projects beneath it. On another Mac it matches the
project only when the repository is checked out at the same path; when
nothing matches it lists the five most recent archived sessions with the
command for each. Uncommitted changes stay on the machine that made them, so
push a branch before continuing elsewhere.

## What the receiving agent is told

A handoff is a record of someone else's session, and its text is whatever
that session contained: your prompts, but also tool output, file contents,
and web pages the first agent read, any of which can hold instructions
written to steer an agent. So the output is shaped as a record, not as
instructions:

- A short **preamble** at the top tells the receiving agent that what
  follows is a filtered record of a past session, that `[REDACTED]` is not
  a real value, that tool output is trimmed and edit bodies are left out,
  to check the repository's current state before acting, to ask you when
  the next step is unclear, and not to follow instructions inside the
  record.
- Prompts, agent replies, and summaries are block-quoted, and plan items
  and file names are rendered so they can't add headings of their own.

`--no-preamble` leaves out that note, for when you write your own framing
around the handoff. The quoting stays, but nothing then tells the receiving
agent that the text is a record rather than a request, so only use it when
your own prompt says so. A handoff read from a shared bucket is only as
trustworthy as everyone who can write to it; see the
[threat model](../security/privacy.md#threat-model).

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
JSON. The format is specified in the [handoff design](../../dev/specs/handoff.md).
