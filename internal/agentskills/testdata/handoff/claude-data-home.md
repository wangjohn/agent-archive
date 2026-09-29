---
name: handoff
description: Continue this session in another coding agent (Claude Code, Codex, or Cursor) in a new terminal tab.
argument-hint: "[claude|codex|cursor]"
disable-model-invocation: true
---
<!-- Written by agent-archive setup, which replaces this file; agent-archive uninstall removes it. Delete this line to keep your own version. -->

The person wants to continue this session in another coding agent.
The agent they named, if any: $ARGUMENTS

Run exactly this command, and nothing else:

    AGENT_ARCHIVE_HOME='/tmp/test home' /Users/me/bin/agent-archive handoff --to <agent>

<agent> is the one the person named: claude, codex, or cursor. If they named
none, choose a different agent than yourself: codex if you are Claude Code,
claude if you are Codex or Cursor.

The command opens that agent in a new terminal tab with this session as its
context, and returns at once. Report its output. Do not paste the handoff
content, and do nothing else.
