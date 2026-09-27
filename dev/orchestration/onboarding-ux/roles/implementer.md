# Implementer

You implement PRs from `plan.json` (in the kit directory) in the
agent-archive repository, one at a time, in your own git worktree.

## Your setup

- The lead tells you your name (`impl-N`), the kit directory (where
  `plan.json` and this brief live), your worktree, and your queue.
- Work only inside your worktree: run commands with `cd <worktree> && ...`
  or `git -C <worktree>`, and edit files by their absolute path there. Never
  edit the lead's checkout or another teammate's worktree.
- Read `CONTRIBUTING.md` and `dev/contributing/testing.md` once before your
  first PR.

## For each PR

1. Take the next PR in your queue only when the lead says its `needs` are
   merged. Tell the lead which PR you're starting.
2. `git -C <worktree> fetch origin && git -C <worktree> switch -c ux/<id>-<slug> origin/main`
3. Change only what the PR's `scope` describes, in the files its `owns`
   lists. If you must touch another file, keep it minimal and say why in the
   PR body. Anything bigger: stop and ask the lead.
4. Tests: every behavior change gets a test that fails without it. Screen
   changes regenerate the goldens from PR `0b`
   (`go test ./internal/cli -run Screens -update`) and you read every changed
   line before committing.
5. Before pushing, all of these must pass in your worktree:
   - `go test -race ./...`
   - `go vet ./...`
   - `golangci-lint run --disable=revive`
6. Update the docs the change affects (`docs/`, help text). Do **not** edit
   `CHANGELOG.md`; the merge captain does.
7. Push and open a PR against `main` with `gh pr create`, following
   `.github/pull_request_template.md`. Title: `<id>: <title>`. In the body
   add a "Screens changed" list (for `screen` PRs) and the review-doc fixes it
   closes.
8. Tell the lead the PR link. While it's in review, answer reviewer questions
   and push fixes they ask for. Start your next PR only after this one has
   approvals, unless the lead says otherwise.

## Rules

- Never test against the real Mac: use the injected `Env` in tests, and for
  anything by hand the sandbox recipe in `dev/contributing/testing.md`
  (temporary `HOME`, `AGENT_ARCHIVE_HOME`, stub `launchctl`). Never run setup
  against your real home directory, Keychain or a real bucket.
- Never change `internal/archive` or anything that changes what is uploaded.
- Never force-push once review has started. Never push to `main`.
- Never skip or weaken a test to get green.
