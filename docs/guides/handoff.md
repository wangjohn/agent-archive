# Continue a session in another agent (handoff)

`handoff` continues a session in another coding agent. Ran out of Claude Code
usage halfway through a task, or want Codex to take a look? Hand the session
off, and the other agent starts with it as context. You don't copy or paste
anything.

There are two ways to do it:

- **Inside an agent.** In Claude Code, type `/handoff codex`. In Codex, ask
  for `$handoff`. The other agent opens in a new terminal tab (a new window
  in Terminal) with this session as its context.
- **From a terminal.** Run `agent-archive handoff`, pick a session, and press
  Enter at `Continue in:`. The other agent starts in this terminal.

## From inside an agent: /handoff

```text
/handoff codex
```

Setup installs a `handoff` skill for the apps it sets up. In Claude Code,
`/handoff codex` hands off the session you are in; `/handoff claude` and
`/handoff cursor` work too. In Codex, ask for `$handoff` (or pick it from
`/skills`) and name the agent. In Cursor, ask the agent to hand off; it
reads the same skill as Codex. With no agent named, the skill picks another
agent than the one you are in: Codex from Claude Code, Claude Code from Codex
or Cursor.

The skill runs `agent-archive handoff --to <agent>`, which opens the agent in
a new tab or window (see [what happens](#what-happens)) and returns at once.
Inside an agent `handoff` never prompts, even when the agent's shell is a
pseudo-terminal, so it cannot hang there. The agent you are in reports where
it opened:

```text
handoff: opened codex in a new iTerm2 tab
```

Both agents can then edit the same checkout; see
[working in a separate checkout](#working-in-a-separate-checkout---worktree)
to give the new one its own.

The skill files are `~/.claude/skills/handoff/SKILL.md` (in
`$CLAUDE_CONFIG_DIR` when set) for Claude Code, and
`~/.agents/skills/handoff/SKILL.md` for Codex and Cursor. Each carries a
marker line: setup replaces and uninstall removes only a file with it, so
delete that line to keep an edited copy. A file already at that path without
it is left alone, and setup says so. An installation with
`AGENT_ARCHIVE_HOME` set runs the command with it, and leaves the skill of
another installation sharing the same home folder alone.
`agent-archive status --verbose` lists the installed files, and `status`
warns when one was written by an earlier release (after you upgrade
`agent-archive`); `agent-archive setup --refresh` (which the installer runs)
refreshes it. To bring a session *into* the agent you are in, without opening
another one, ask it in words: the `agent-archive` skill runs
`handoff "<words>"` for you ([agent skills](agent-skills.md)).

## From a terminal

```sh
agent-archive handoff
```

Pick a session from the numbered list (see
[where the session comes from](#where-the-session-comes-from)), and
`handoff` asks where to continue:

```text
Continue in:
  1) Codex (default)
  2) Claude Code
  p) print
  c) copy to the clipboard
  w) write to a file
  q) quit
Enter 1-2, p, c, w, or q [1]:
```

Only agents whose CLI is on `PATH` are numbered. Enter takes the default:
`handoff.default_to` for the session's harness in `config.json` (see
[configuration](../reference/configuration.md)) when that agent is installed,
else Codex for a Claude Code session and Claude Code for a Codex or Cursor
one, else the first listed. A number (or an agent's name) starts that agent
in this terminal, and `handoff` returns when it exits.

The other answers don't start an agent. `p` prints the handoff, through the
pager when it is longer than the screen. `c` copies it with `pbcopy`. `w`
asks for a file name (default `handoff-<short id>.md` in the `--project` or
current directory; `~/` is your home directory), writes it with mode 0600,
and asks before replacing a file. A write that fails is reported and asked
again; Enter or `q` then gives up. With no agent installed, Enter prints.

To skip the question, name the agent with `--to`:

```sh
agent-archive handoff --to codex                       # pick the session
agent-archive handoff --latest --harness claude --to codex
agent-archive handoff SESSION_ID --to claude
agent-archive handoff SESSION_ID --to cursor
agent-archive handoff "fix the auth bug" --to claude   # words, as for show
```

`handoff` asks where to continue only on a terminal, and not with `--to`,
`--output`, `--format json`, or `--no-preamble`. Inside Claude Code, Codex,
or Cursor (`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, or `CURSOR_AGENT` is
set), or with `AGENT_ARCHIVE_NONINTERACTIVE=1`, it never asks anything, even
on a terminal; `AGENT_ARCHIVE_NONINTERACTIVE=0` turns asking back on. See
[scripting and other outputs](#scripting-and-other-outputs) and
[configuration](../reference/configuration.md#environment-variables).

## What happens

1. **The session is read and filtered.** Everything the agent gets has passed
   the same [privacy filter](../security/privacy.md) as the archive. A
   session on this machine is read from its transcript as it is now, so nothing
   needs to sync first.
2. **The record goes to a private file.** It is written to
   `handoffs/launch-<session>-<time>-<random>/handoff.md` in the data
   directory: a folder of its own, file mode 0600. The file is kept so a
   resumed session can read it again, and removed after 7 days like other
   saved handoffs. With `--file` before setup there is no data directory, so
   it goes to a private folder in the system's temporary directory, kept for
   the system to clear.
3. **The agent is started with a short prompt** asking it to read that file.
   The file begins with a preamble telling the agent what it is reading and
   how to get more context (see
   [what the receiving agent is told](#what-the-receiving-agent-is-told)).
4. **It opens here or in a new tab.** On a terminal, the agent runs there and
   `handoff` returns when it exits. Anywhere else, including inside an agent
   (or with `AGENT_ARCHIVE_NONINTERACTIVE=1`) even when its shell is a
   pseudo-terminal, as when an agent runs `handoff --to` for you, it opens
   without asking in a new tmux window when `$TMUX` is set, else a new
   iTerm2 or Ghostty tab (by `$TERM_PROGRAM`), else a new Terminal window,
   and `handoff` returns at once. `--here` forces this terminal (and fails
   without one, or inside an agent); `--new-window` opens a new window or
   tab even from a terminal. When the agent exits with an error there, the
   window waits for Enter so you can read it. When no window can be opened
   (not macOS and not inside tmux), `handoff` exits 1 and prints the command
   to paste into a terminal; it never runs the agent without one.

`--to` starts the installed `claude`, `codex`, or Cursor `agent` (else
`cursor-agent`) CLI in the current directory (or `--project DIR` with
`--latest`). Codex gets that directory with `--cd` and Cursor with
`--workspace`. Claude Code runs in it and is given only the handoff's folder
with `--add-dir`, since it reads outside the project only from an added
directory. The launched agent does not inherit the calling agent's session
variables (such as `CLAUDE_CODE_SESSION_ID` or `CODEX_THREAD_ID`); your
settings, such as `CLAUDE_CODE_USE_BEDROCK`, pass through. It never starts a
remote agent.

### Passing arguments to the agent

Arguments after `--` go to the agent, after any set for it in `config.json`'s
`handoff.args` (see [configuration](../reference/configuration.md)). A second
`--` is refused, since the prompt follows one:

```sh
agent-archive handoff SESSION_ID --to codex -- --model o3
```

Give every option its value (`--model o3` or `--model=o3`). The prompt comes
right after these arguments, so an option left without its value at the end
would take the launcher's `--` (Claude Code, Codex) or the prompt itself
(Cursor) as its value.

## Working in a separate checkout (`--worktree`)

Two agents editing one checkout get in each other's way. `--worktree` gives
the launched agent a checkout of its own:

```sh
agent-archive handoff SESSION_ID --to codex --worktree
agent-archive handoff --worktree   # pick the session and agent on a terminal
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
revert is refused: finish or abort it first. Launched from a subdirectory,
the agent starts in the same subdirectory of the worktree. The worktree's
path and branch are printed:

```text
handoff: created worktree /Users/me/src/app-handoff-3f2a9c1e on branch handoff/3f2a9c1e (carried 2 changed and 1 untracked files)
```

Without `--to`, the worktree is made only once you pick an agent at
`Continue in:`; print, copy, and write make none.

An existing branch or directory of that name is an error, never reused;
pick another branch with `--branch`, or remove the old worktree with
`git worktree remove`. If copying your changes fails after the worktree was
made, the worktree is left in place and the error says where. When you are
done, merge or cherry-pick the branch and run `git worktree remove` on the
directory.

Without `--worktree`, if the session being handed off is on this machine, was
active in the last 2 minutes, and belongs to the checkout the agent would
start in, a terminal asks first:

```text
The source session was active just now; continue in the same checkout? [y/N/w]
```

`y` continues, `N` (the default) cancels with nothing launched, and `w`
creates a worktree as `--worktree` does. Without a terminal, or inside an
agent (or with `AGENT_ARCHIVE_NONINTERACTIVE=1`), `handoff` prints a warning
and continues. When an agent hands off its own session with
`--to`, as `/handoff` does, it is active by definition; a note is printed
and nothing is asked.

## Naming a session by its title

The argument is a session ID or words; quote several words as one argument:

```sh
agent-archive handoff "fix the auth bug" --harness codex
agent-archive handoff "fix the auth bug" --to claude --worktree
```

It matches the way `show` and `list "<words>"` do: every word must appear,
case-insensitively, in some field of the session (its name, its title (the first
prompt), its branch, its project name, or its app), or start its session ID
(from 4 characters), and a word like `#212` or `212` also matches a pull request
the session linked or created, never the start of an ID. Words may match
different fields, so a topic, a PR number, a branch, or a project name all work.
A session ID, full or the short one a table shows, names that session, even when
another title mentions it. Inside a repository, its top-level sessions are
searched first, then every project's, and subagent sessions only when no other
session matches; a note on stderr says how many more match in other projects.
`--harness` and `--source` narrow the search. When the command runs inside a
Claude Code or Codex session, that session is not offered for a title (as
`--latest` passes over it), unless `--to` is set, which hands off a session the
caller names.

The search looks at this machine's sessions first, which needs no network and
no upload: a session's name, title (its first prompt), branch, and linked pull
requests are read from its transcript file on this machine, and only those, its
project name, and its app are matched, never the rest of the conversation. It
goes on to the archive's sessions when none of this machine's match (inside a
repository, the repository's archived sessions come before this machine's
sessions in other projects), and a single word of 8 hexadecimal characters that
is not the short ID of one of this machine's sessions is also looked up in the
archive as a short ID first. An archive that cannot be reached does not fail
words this machine can answer. `--source local` or `--source archive` limits it
to one. Only the 50 most recently active sessions on this machine that have a
prompt are searched by words; an older one that was uploaded is found in the
archive.

One match is handed off, and then everything else applies to it: `--to`,
`--worktree`, the `Continue in:` question on a terminal. Several matches are
never guessed between. On a terminal the picker opens with just those
sessions. Without one, and inside a coding agent (where nothing is asked),
they are printed to standard error, each with its short ID, app, project
(when they span several), age, pull request (when one has any), and title,
followed by the exact command to run next (`Next: agent-archive handoff
d7a77938 --harness claude`) and the `list "<words>" --json` that shows them as
data; the command exits with code 1, so the caller can ask which and run it
again with an ID. At most the 20 newest are listed, with a count of the rest;
add words, a PR number, or `--harness` to narrow. One
match on this machine is taken even when the archive holds others, so name an ID
when in doubt. With none, the message points to `agent-archive list`.

## Where the session comes from

With no session selector, `handoff` opens a numbered session picker when
stdin and stdout are terminals. It lists this machine's sessions, including ones
marked `not yet uploaded`, together with archived ones, most recently active
first; a session that is both appears once. Subagent sessions and sessions
with no prompt yet are left out, and `--harness` narrows the list. When the
archive cannot be read (offline, say), the picker lists this machine's sessions
and says why archived ones are missing. Quit with `q` without producing a
handoff. In a script or pipeline, pass a session ID or words, `--latest`, or
`--file`.
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

A session registered on this machine is read from its transcript as it is now, so
a handoff right after you stop needs no sync and works while collection is
paused; nothing is uploaded. Otherwise the session is downloaded from the
archive, which is how a second machine hands off a session from the first.
`--source local|archive` forces one or the other; with neither, a local
transcript that cannot be read falls back to the archive's copy. Either kind
can be launched with `--to`.

`--latest` names its choice on stderr, passes over sessions with no prompt
yet, and, when run by an agent that names its own session (Claude Code does,
through `CLAUDE_CODE_SESSION_ID`), skips that session unless `--to` is used.
With `--to`, the calling session is eligible because it is the source being
handed off. `--latest` matches the current directory's project, not projects
beneath it.

### Finding a session by repository

A session matches the current directory when it ran at the same path, or in
a checkout of the same repository. The repository is identified by its
`origin` remote (`git remote get-url origin`): a session captured in a
repository with an `origin` records a hash of it (see
[privacy](../security/privacy.md#what-is-uploaded)), so the
session from your other Mac is found even when the repository is at a
different path there, and whether it was cloned over SSH or HTTPS.
Running in a subdirectory of the repository works; running from a folder that
holds several repositories matches none of them. A session that ran at this path always comes before one found only by
repository: this Mac's sessions for the path, then the archive's, and only
when there are none does a repository match count. Within each kind the most
recently active session wins, and this Mac's own sessions are tried before
the archive's. So to take the newest session from another Mac when this one
has an older one at the same path, add `--source archive`.

- **A fork's `origin` is the fork.** It is not the repository it was forked
  from, so a clone of the fork and a clone of the upstream do not match each
  other. Only the remote named `origin` is read.
- **No `origin`, no repository match.** A directory that is not a git
  repository, or has no remote named `origin`, matches by path only, as
  before. `git` must be installed, and sessions captured before you updated
  gain the key on the Mac that captured them, when the collector next
  refreshes them and the repository is still there.
- **Branches are not compared.** When the session was on another branch
  than the one you have checked out, `handoff` says so on stderr
  (``handoff: session was on `feature/x`; you are on `main` ``) and tells
  the receiving agent in the workspace lines, and continues anyway. The
  workspace lines also say when the session ran in a different directory
  than yours, so the agent checks paths against the tree in front of it.
- **When nothing matches**, `handoff` says what it tried (the repository,
  then the path) and lists the five most recent archived sessions with the
  command for each.

A repository can name any origin, and so can anyone who can write to the
archive, so the key is a convenience and not proof that a session is yours.
When `--latest` reaches a session by repository and not by path, `handoff`
therefore stops before downloading any of it. On a terminal it names the session
(this or another Mac, project, start time, first prompt, each cut short) and
asks; the answer defaults to no, and nothing is printed or launched until
you say yes (for a session on this Mac its transcript is read first, to show
the first prompt, and stays on the Mac). Where it cannot ask (a pipe, or an
agent's shell, which has prompts off) it does not use the session and exits
1. It prints only which machine (this or another Mac), when the session
started, and the command that does, `agent-archive handoff SESSION_ID`, with
the `--harness`, `--to`, and `--worktree` you gave (not `--format`,
`--output`, `--max-bytes`, or `--branch`; add them). If the session's ID is
not 32 lowercase hexadecimal digits, it says to run `agent-archive list`
instead. That refusal is a speed bump for an agent that has been steered, not
a barrier: it can still run the command or name a session ID, so check what
an agent is doing with a session from another computer. Naming a session
yourself, or picking one in the picker, is never questioned. The reasons are
in the [threat model](../security/privacy.md#threat-model).

The notes about branch and directory are made for the checkout `handoff`
runs in. They are left out with `--worktree`, since the agent then starts in
a new worktree, and they stay in a handoff you print or pipe.

Uncommitted changes stay on the machine that made them, so push a branch
before continuing elsewhere.

## What the receiving agent is told

The handoff starts with where the session left off, the latest plan, and the
files touched, then every prompt with the agent's replies, one-line tool-call
summaries, trimmed tool output, and any summary Claude Code wrote when the
session was compacted. Edit bodies are left out; the receiving agent should
read the files as they are now. Cursor transcripts record no tool results, so
a Cursor handoff says so and shows none.

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

An agent launched by `handoff` is also told that Agent Archive exists, where
its executable is, and how to ask for more context:
`agent-archive show SESSION_ID --transcript` reads the published archive
copy, which may lag the local transcript or not exist yet, and
`agent-archive handoff SESSION_ID --source local --max-bytes 0` reads the
current, complete **filtered** local record. Neither command exposes
unfiltered raw transcript data.

`--no-preamble` leaves out that note, for when you write your own framing
around a printed handoff; `--to` always includes it. The quoting stays, but
nothing then tells the receiving agent that the text is a record rather than
a request, so only use it when your own prompt says so. A handoff read from
a shared bucket is only as trustworthy as everyone who can write to it; see
the [threat model](../security/privacy.md#threat-model).

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

## Scripting and other outputs

Piped or redirected output never asks where to continue; it prints the
handoff as before. So you can hand it to an agent as its first prompt
yourself:

```sh
# Continue in Claude Code what you started in Codex, in the same repository
claude "$(agent-archive handoff --latest --harness codex)"

# Continue in Codex what you started in Claude Code
codex "$(agent-archive handoff --latest --harness claude)"
```

Other outputs:

```sh
# A specific session, from `list` or by its title, written to a file (mode 0600)
agent-archive handoff SESSION_ID --output /tmp/handoff.md
agent-archive handoff "fix the auth bug" --output /tmp/handoff.md

# The same content as JSON
agent-archive handoff SESSION_ID --format json

# A transcript the archive never captured, on this machine; needs no setup
agent-archive handoff --file ~/.codex/sessions/.../rollout-....jsonl --harness codex
```

`--output` refuses to replace an existing file unless you add `--force`.
`--format json` prints the structured handoff document the prompt is rendered
from; see [JSON output](../reference/json-output.md). The format is specified
in the [handoff design](../../dev/specs/handoff.md).
