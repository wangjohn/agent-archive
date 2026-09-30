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

`--to` launches the installed local `claude`, `codex`, or Cursor `agent`
(else `cursor-agent`) CLI in the current directory (or `--project DIR` with
`--latest`). It writes the filtered handoff to
`handoffs/launch-<session>-<time>-<random>/handoff.md` in the data directory (a
private folder of its own, file mode 0600) and sends the agent a short
prompt to read it. The file is kept so a resumed session can read it again,
and removed after 7 days like other saved handoffs. With `--file` before setup there is no data directory, so the file
goes to a private folder in the system's temporary directory, kept for the
system to clear. Claude Code is given only that folder with `--add-dir`,
since it reads outside the project only from an added directory.
The launched agent does not inherit the calling agent's session variables
(such as `CLAUDE_CODE_SESSION_ID` or `CODEX_THREAD_ID`); your settings, such as
`CLAUDE_CODE_USE_BEDROCK`, pass through.

### Working in a separate checkout (`--worktree`)

Two agents editing one checkout get in each other's way. `--worktree` gives
the launched agent a checkout of its own:

```sh
agent-archive handoff SESSION_ID --to codex --worktree
agent-archive handoff SESSION_ID --to claude --worktree --branch try-codex-fix
```

It runs `git worktree add -b handoff/<id> <repo>-handoff-<id> HEAD`, where
`<id>` is the first 8 characters of the session ID and `<repo>-handoff-<id>`
is a directory beside the checkout; `--branch NAME` names the branch instead.
Your uncommitted changes and untracked files come along: the changes are
recorded with `git stash create` and applied in the new worktree, which
leaves your checkout and your stash list untouched, and untracked files are
copied with their permissions (symlinks as symlinks). Changes you had staged
arrive unstaged, except new files, which arrive staged. Ignored files, such
as `.env` or `node_modules`, are not copied, and submodules are neither
checked out nor carried (run `git submodule update --init` in the
worktree). A checkout in the middle of a merge, rebase, cherry-pick, or
revert is refused: finish or abort it first. Launched from a subdirectory, the agent starts in the same
subdirectory of the worktree. The worktree's path and branch are printed:

```text
handoff: created worktree /Users/me/src/app-handoff-3f2a9c1e on branch handoff/3f2a9c1e (carried 2 changed and 1 untracked files)
```

An existing branch or directory of that name is an error, never reused;
pick another branch with `--branch`, or remove the old worktree with
`git worktree remove`. If copying your changes fails after the worktree was
made, the worktree is left in place and the error says where. When you are
done, merge or cherry-pick the branch and run `git worktree remove` on the
directory.

Without `--worktree`, if the session being handed off is on this Mac, was
active in the last 2 minutes, and belongs to the checkout the agent would
start in, a terminal asks first:

```text
The source session was active just now; continue in the same checkout? [y/N/w]
```

`y` continues, `N` (the default) cancels with nothing launched, and `w`
creates a worktree as `--worktree` does. Without a terminal, `handoff` prints
a warning and continues. When an agent hands off its own session with
`--to`, it is active by definition; the note is printed once and nothing is
asked.

Arguments after `--` go to the agent (a second `--` is refused, since the
prompt follows one), after any set for it in `config.json`
(see [configuration](../reference/configuration.md)):

```sh
agent-archive handoff SESSION_ID --to codex -- --model o3
```

The session is read as for any handoff: this Mac's transcript when there is
one, else the archive's copy. It never starts a remote agent. The receiving
agent is told that Agent Archive exists, where to
find its executable, and how to ask for more context.
`agent-archive show SESSION_ID --transcript` reads the published archive copy,
which may lag the local transcript or not exist yet.
`agent-archive handoff SESSION_ID --source local --max-bytes 0` reads the
current, complete **filtered** local record. Neither command exposes
unfiltered raw transcript data. The handoff uses the usual 120,000-byte
default budget; `--max-bytes 0` includes all filtered content.

## Where the session comes from

With no session selector, `handoff` opens a numbered session picker when
stdin and stdout are terminals. It lists this Mac's sessions, including ones
marked `not yet uploaded`, together with archived ones, most recently active
first; a session that is both appears once. Sessions with no prompt yet are
left out, and `--harness` narrows the list. When the archive cannot be read
(offline, say), the picker lists this Mac's sessions and says why archived
ones are missing. Quit with `q` without producing a handoff. In a script or
pipeline, pass a session ID, `--latest`, or `--file`.

With `--to` and no selector, run from inside an agent, `handoff` hands off
the session it is running in without asking: the one Claude Code names in
`CLAUDE_CODE_SESSION_ID` or Codex in `CODEX_THREAD_ID`. Cursor names no
session, so inside Cursor (`CURSOR_AGENT` is set) it takes the newest Cursor
session for the current directory, as `--latest --harness cursor` would.
Otherwise a terminal gets the picker, and anything else is told to name a
session.

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
