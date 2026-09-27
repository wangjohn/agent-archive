# Onboarding UX team

Runs the onboarding UX implementation plan (16 PRs, from the
[onboarding review](https://claude.ai/code/artifact/55a4af8d-8887-494d-a66c-800d6655df8b))
with a team of Claude Code sessions on your own Mac: one lead, five
implementers and three reviewers. Each teammate is its own session in its
own terminal pane, working in its own git worktree.

| File | What it is |
| --- | --- |
| `plan.json` | The 16 PRs: scope, files each owns, dependencies, who does what. |
| `lead.md` | The lead session's brief: build the task list, spawn the team, run it. |
| `roles/implementer.md` | The five implementers' brief. |
| `roles/reviewer-correctness.md` | Reviews every PR for bugs and tests; fixes small things. |
| `roles/reviewer-ux.md` | Reviews every PR that changes a screen: color, layout, wording. |
| `roles/merge-captain.md` | The only one who merges; keeps `main` green and in order. |
| `setup-worktrees.sh` | Creates the eight worktrees beside this checkout. |

## Before you start

- Claude Code with agent teams, which are experimental: they are enabled by
  `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1` and need an interactive session
  (they don't run under `claude -p`). See
  [agent teams](https://code.claude.com/docs/en/agent-teams.md).
- tmux or iTerm2, so each teammate gets its own pane.
- The repo's toolchain: Go 1.27.1, Xcode command line tools,
  golangci-lint v2.14.0 ([CONTRIBUTING.md](../../../CONTRIBUTING.md)).
- `gh`, logged in with push access to this repository.
- Nine Opus sessions for several days uses a lot of tokens; check your plan's
  limits first.

## Run it

```sh
cd path/to/agent-archive
git fetch origin
git switch claude/agent-archive-cli-review-j2k494   # the branch with this kit
dev/orchestration/onboarding-ux/setup-worktrees.sh
tmux new -s aa-team        # skip if you use iTerm2
caffeinate -i &            # keep the Mac awake while the team works
CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 claude --model claude-opus-5-5 --teammate-mode tmux
```

Use `--teammate-mode iterm2` in iTerm2. Then tell the lead:

> Read dev/orchestration/onboarding-ux/lead.md and follow it.

The lead creates one task per PR with its dependencies, spawns the eight
teammates, and keeps a status table. Your checkout stays on the kit's branch
and is where the lead runs and the briefs are read from; the worktrees start
from `origin/main`, and every PR targets `main`.

## While it runs

- Every teammate has its own pane; click into one to watch it or type to it
  directly.
- The lead stops for you three times, on PRs `0a` (color system), `E1`
  (status) and `F1` (review screen), with before/after screenshots on the PR.
  Reply "OK to merge" or say what to change.
- Ask the lead for status at any time. It also reports any PR stuck for
  2 hours.
- To pause, tell the lead to have everyone stop after their current step. To
  stop for good, ask it to shut the team down.

## Clean up

```sh
dev/orchestration/onboarding-ux/setup-worktrees.sh --remove
```

## Guardrails built into the briefs

- Each implementer works only in its own worktree and only on the files its
  PR owns.
- Only the merge captain merges, only when CI is green on Linux and macOS,
  the reviewers approved, and the PR's dependencies are merged.
- Nobody changes `internal/archive` (what gets uploaded), pushes to `main`,
  force-pushes after review starts, or skips a test.
- Nothing runs against your real home directory, Keychain, LaunchAgent or
  bucket: tests use the injected `Env`, and anything by hand uses the
  sandbox in [testing](../../contributing/testing.md).
