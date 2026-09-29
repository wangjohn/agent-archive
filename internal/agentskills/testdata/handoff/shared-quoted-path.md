---
name: handoff
description: Continue this session in another coding agent (Claude Code, Codex, or Cursor) in a new terminal tab. Use only when the person explicitly asks to hand off.
---
<!-- Written by agent-archive setup, which replaces this file; agent-archive uninstall removes it. Delete this line to keep your own version. -->

The person wants to continue this session in another coding agent.
Only if they explicitly asked you to hand off, or to continue in another
agent, run the command below; otherwise run nothing.

Run exactly this command, and nothing else:

    '/Users/me/My Tools/agent-archive' handoff --to <agent>

<agent> is the one the person named: claude, codex, or cursor. If they named
none, choose a different agent than yourself: codex if you are Claude Code,
claude if you are Codex or Cursor.

The command opens that agent in a new terminal tab with this session as its
context, and returns at once. Report its output. Do not paste the handoff
content, and do nothing else.
