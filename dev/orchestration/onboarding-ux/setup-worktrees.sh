#!/bin/sh
# Create the eight worktrees the onboarding UX team works in, next to this
# checkout, each detached at origin/main. Safe to rerun: existing worktrees
# are left as they are. Remove them all with: ./setup-worktrees.sh --remove
set -eu

root=$(git rev-parse --show-toplevel)
parent=$(dirname "$root")
names="aa-impl-1 aa-impl-2 aa-impl-3 aa-impl-4 aa-impl-5 aa-rev-1 aa-rev-2 aa-rev-3"

if [ "${1:-}" = "--remove" ]; then
  for name in $names; do
    if [ -d "$parent/$name" ]; then
      git -C "$root" worktree remove "$parent/$name" && echo "removed $parent/$name"
    fi
  done
  git -C "$root" worktree prune
  exit 0
fi

for tool in git gh go golangci-lint; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing: $tool" >&2; exit 1; }
done
gh auth status >/dev/null 2>&1 || { echo "run: gh auth login" >&2; exit 1; }

git -C "$root" fetch origin main
for name in $names; do
  path="$parent/$name"
  if [ -d "$path" ]; then
    echo "exists: $path"
  else
    git -C "$root" worktree add --detach "$path" origin/main >/dev/null
    echo "created: $path"
  fi
done
echo
echo "Next: from $root run"
echo "  CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 claude --model claude-opus-5-5 --teammate-mode tmux"
echo "and tell it: Read dev/orchestration/onboarding-ux/lead.md and follow it."
