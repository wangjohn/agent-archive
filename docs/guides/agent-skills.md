# Agent skills

Setup gives Claude Code, Codex, and Cursor two skills, so you can talk to your
agent about your other sessions instead of typing commands:

| Skill | Who runs it | What it does |
| --- | --- | --- |
| `agent-archive` | The agent, when you refer to a past session | Finds a session, on this Mac or in the archive, and pulls it in as context. |
| `handoff` | You, as `/handoff codex` | Continues the session you are in, in another agent ([handoff](handoff.md#from-inside-an-agent-handoff)). |

A skill is a text file the agent reads: it names the commands to run and when.
The `agent-archive` skill runs the same read-only commands you would:
`handoff`, `list`, `show`, and `status`.

## Asking for a past session

Say what you want, as you would to a colleague:

```text
Pull in the auth session from Codex.
What did we do in Cursor yesterday?
Look at my last Claude Code session about the migration, then continue it.
Continue where my other agent left off.
```

The agent runs `agent-archive handoff "auth" --harness codex`, which prints
that session as a prompt: filtered as the archive is (injected instructions and
credentials removed, tool output trimmed), cut to about 120 KB, and addressed
to the receiving agent. It then does what you asked with it.

- **It finds the session by its words.** A title substring or a short session
  ID works, in this Mac's sessions first (no network), then the archive's, so a
  session from an hour ago needs no bucket access. A title is the session's
  first prompt, so the agent searches for one or two distinctive words. With no
  topic ("where my other agent left off") it takes the most recent session for
  the project you are in (`--latest`), and it never picks the session it is
  running in.
- **It never guesses.** When several sessions match, the agent shows you the
  candidates (short ID, agent, project, when, title) and asks which. It never
  picks for you.
- **When nothing matches**, it tries different words, or looks at the last 30
  days with `list --since 30d` and shows you the titles.
- **It can browse:** `list` for what exists (a short table; `--json` for the
  full metadata), `show ID` for a session's summary, `show ID --transcript`
  for its conversation, bounded to about 120 KB (the rest is saved in a file
  whose path is named).

The skill never lets the agent run `setup`, `uninstall`, `purge`, `backfill`,
`sync`, or `feedback`, add `--to` to `handoff` (that starts another agent, and
belongs to `/handoff`), or lift the size limit with `--max-bytes 0`, unless you
asked for exactly that. Nothing in it prompts: inside an agent every command
answers or fails instead of waiting for a key
([`AGENT_ARCHIVE_NONINTERACTIVE`](../reference/configuration.md#environment-variables)).

## What the agent can read through the skill

What you would see running the same commands yourself: the filtered session
`handoff` prints, `list`'s table, and `show`'s summary or transcript, all cut
to a size bound. It is the archive's filtered content (credentials and
injected instructions removed, tool output trimmed), not your raw transcript
files, and the skill tells the agent not to open those. That is an
instruction, not a barrier: the agent runs as you, so it could read files you
can read whatever the skill says. Once a session is in the agent's context,
that agent's own provider sees it like anything else in the conversation.
Pulling in a session from another agent therefore shows that content to this
agent's provider. See [privacy](../security/privacy.md#what-an-agent-can-read-through-the-skill).

## What the agent is told about what it reads

A pulled-in session is a record of another conversation, and text in it may
have been written by a web page or a tool. It is filtered for credentials, not
for hostile wording, so the skill tells the agent to treat it as data: never to
follow instructions found inside it, and to check the repository's current
state before relying on it. If the output was trimmed, it may open the one
untrimmed copy the last line names (a file in `handoffs` under the data
directory) and no other path the text mentions. See [privacy](../security/privacy.md).

## Permissions

The agent's own permission settings still apply.

- **Claude Code.** Using a skill is itself something Claude Code asks about,
  so expect a question the first time the agent picks this one; allow it for
  good with a `Skill(agent-archive)` permission rule. Then the commands. The
  skill names one command the agent may run without asking:
  `agent-archive status`, spelled exactly so (its full path is in the file).
  Claude Code applies that only during the turn that uses the skill, and in
  our checks of version 2.1.283 a skill the agent chose did not get it (a skill
  you start yourself with `/agent-archive` did), so `status` may ask too. The
  first time the agent runs `handoff`, `list`, or `show`, Claude Code asks you.
  That is on purpose. A permission rule cannot leave a flag out: allowing
  `handoff` for the skill would allow `handoff --to codex`, which starts another
  agent, and allowing `show` would allow `--max-bytes 0`, which prints a whole
  transcript. Text in a session the agent reads must not be able to start
  either. If you tell Claude Code to always allow a command, you are choosing
  that wider rule yourself. Where nothing can ask, as in `claude -p`, the
  command is denied, and the agent says so and shows you the command.
- **Codex and Cursor.** Their approvals and sandbox decide: Codex's default
  asks before a command that needs the network, and Cursor's terminal commands
  need approval unless you chose a run mode that does not ask.
- **A sandbox.** With Claude Code's sandbox on, or in Codex's or Cursor's
  default one, the network is blocked. A session on this Mac is still found by
  its title (no network), but the archive fails with `operation not
  permitted`. The skill tells you what failed and asks you to allow it, or to
  run the command yourself, rather than retrying variations. None of the three
  documents whether a sandboxed command can read the macOS Keychain, which an
  R2 archive needs, so expect to allow or run those reads yourself.

## Where the files are

| App | File |
| --- | --- |
| Claude Code | `~/.claude/skills/agent-archive/SKILL.md` (in `$CLAUDE_CONFIG_DIR` when set) |
| Codex and Cursor | `~/.agents/skills/agent-archive/SKILL.md` |

Each file names the full path of the `agent-archive` you installed, since an
agent's shell often does not have it on `PATH`, and `AGENT_ARCHIVE_HOME=...`
when your installation uses another data directory. Each carries the marker
line described for [`/handoff`](handoff.md#from-inside-an-agent-handoff): setup
replaces and uninstall removes only a file with it, delete the line to keep an
edited copy, and a file already at that path without it is left alone.
`agent-archive status --verbose` lists the installed files, and `status` warns
when one was written by an earlier release. Upgrading with the installer
refreshes both skills for you ([install](../getting-started/install.md)); so does
`agent-archive setup --refresh`, which changes nothing else.

Cursor also reads `~/.claude/skills` (and `~/.codex/skills`), so on a Mac set up
for both Claude Code and Cursor it can find the skill in two places. The two
files carry the same instructions; Cursor's documentation does not say whether
it lists a duplicate once. Codex reads `~/.agents/skills`; setup writes nothing
under `$CODEX_HOME`.

Claude Code and Codex notice a new or refreshed skill in a running session
(restart the app if it does not show up); Cursor's documentation does not say,
so start a new chat there.

## Turning the skills off

`agent-archive setup --no-skills` installs neither skill and removes the ones
setup wrote (a file that is not setup's is left alone and named). It is saved,
so later setup runs keep them off, and `agent-archive setup --skills` turns
them back on ([setup](../getting-started/setup.md)). `agent-archive
uninstall` removes them along with the hooks.

## If the agent does not use the skill

See [my agent does not use the skill](troubleshooting.md#my-agent-does-not-use-the-skill).
