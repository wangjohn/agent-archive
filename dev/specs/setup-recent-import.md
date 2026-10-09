# Import the last 7 days at setup — engineering plan

Status: planned 2026-10-08; [decisions](#decisions) confirmed by the owner
the same day. §1–§3 implemented in
[#386](https://github.com/wangjohn/agent-archive/pull/386)
(`importRecentSessions` in `internal/cli/backfill.go`, `backfill.Plan.Recent`
in `internal/backfill/recent.go`); §4 (`status`) not started. Where this plan
and the code differ once it merges, the code is the reference and
differences go under Deviations.

Goal: the session a person is working in when they set up agent-archive,
and anything else they did in the past week in the projects they chose, is
in the archive when setup finishes, without a question and without knowing
`backfill` exists.

## Decisions

Confirmed 2026-10-08:

1. **Setup imports the last 7 days automatically.** No prompt, in both
   interactive setup and `setup --yes`. The window is the 7 local days
   before setup, the same as `backfill --since 7d`, over the projects and
   apps setup just included.
2. **Discovery's start-time floor stays.** Automatic discovery still admits
   only sessions that start after it is enabled. The lookback is a
   one-time import at setup, not a wider discovery rule, so the consent
   moment stays setup. Once a session is imported, discovery keeps it
   current: `admitOwnedCandidate` admits a session it already has an
   archive ID for even when `DiscoveryGeneration` would refuse the start
   (`internal/discovery/discovery.go`, the `!authorized` branch).

## Problem

Found 2026-10-08 trying to hand off a Codex session ("Diagnose Codex
background capture") that `handoff` could not find:

1. The session started at 2026-10-05 21:09 UTC. Codex discovery was enabled
   for every project at 21:43 UTC, 34 minutes later, very likely from
   inside that session. Discovery read the rollout, classified it
   `native_format`, and then refused it as `start_not_authorized`
   (`config.DiscoveryGeneration`). It kept working in that session for
   three more days and none of it was captured.
2. Nothing said so. `status` showed no line for the session, `list` and
   `handoff` reported no match, and the next-steps text ("Sessions already
   open are not captured") is easy to miss and offers no fix.
3. The fix exists but is hidden. `agent-archive backfill --since 7d
   --harness codex --project ~/agent-archive --dry-run` plans exactly this
   one session (36 MB, started 2026-10-05). Interactive setup already
   offers a full-history import at the end (`offerSetupImport`), but it is
   the last step after next steps, it is all history or nothing, and
   `setup --yes` only prints a pointer to `backfill`.

The person running setup is very often inside a long agent session at that
moment. That session is the one most likely to start just before the floor
and the one they most want to hand off.

## Design

### 1. Setup runs a 7-day import

`finishSetup` replaces `offerSetupImport` with `importRecentSessions`:

- Builds the plan exactly as `offerSetupImport` does today
  (`backfill.BuildPlan` with `Filters{Harnesses: cfg.Harnesses, Projects:
  included roots}`), plus `Since` set to 7 local days before now (the value
  `--since 7d` resolves to).
- If the plan imports anything, it commits the batch the way
  `backfill --yes --background` does: register the sessions and return;
  the background collector uploads them. Setup does not wait on uploads.
- Prints one line: `Imported N sessions from the last 7 days (Codex N,
  Claude Code N). Uploading in the background.` and, when there are older
  sessions in the same projects, a second line: `N older sessions: run
  agent-archive backfill to import them.`
- Never adds projects. The filter is the included roots, so the backfill
  rules that propose new projects (plain folders, temporary directories,
  recovered worktrees) cannot fire. Recovery proposals that would add a
  root are dropped from this import and left to `backfill`.
- Skips silently when capture is paused (backfill refuses too), there are
  no included projects or apps, or `importRefusal` says no.
- Planning or commit failure prints `Recent sessions were not imported:
  <reason>. Run agent-archive backfill --since 7d to retry.` and setup
  still succeeds. Ctrl-C during planning behaves as today.
- Runs the same in `setup --yes`. `setupFinish.offerImport` goes away.

The import is an ordinary import batch, so `backfill history` lists it and
`backfill undo` reverses it, and retention applies as for any import.

### 2. Older history

Setup no longer offers a full-history import. The count of older sessions
comes from the same plan with no `Since` (one extra filter pass over the
same discovery; no second store scan). If that costs too much on large
stores, drop the count and print the pointer only.

### 3. Next steps text

`printNextSteps` drops "Sessions already open are not captured. Start a new
one in an included project." Sessions open now are covered by the import,
and discovery keeps them current. The line becomes "Check progress with
agent-archive status." alone. The `unattended` pointer to `backfill` goes
away, since `--yes` imports too.

### 4. People who already ran setup

They are past the moment this fixes. `status` gains one line per app, only
when it applies:

```
! 1 Codex session from the 7 days before setup was not captured.
  Import it with agent-archive backfill --since 2026-09-28.
```

The date is 7 days before the earliest discovery or capture authorization
for that app. The count is sessions discovery classified as capturable and
refused as `start_not_authorized` in the last scan, so it costs nothing
extra. It disappears once they are imported or fall outside the window.

## Tests

- Setup (interactive and `--yes`) with sessions 2, 6, and 9 days old in an
  included project imports the first two, says one older session remains,
  and asks no question.
- A session that started before setup and keeps writing after it is
  imported and then updated by discovery (new turns reach the archive).
- No projects are added: a session in an unconfigured folder, a temporary
  directory, and a missing worktree are not imported and not proposed.
- Paused capture, no included projects, and a planning error each leave
  setup successful with the stated output.
- `backfill undo` of the setup batch removes exactly those sessions.
- `status` shows the pre-setup line when discovery refused a recent session
  with `start_not_authorized`, and not otherwise.
- Golden output for setup's final screen.

## Sequencing

One PR for §1–§3 (setup and its goldens), one for §4 (`status`). Either
order works; §4 helps existing installs first.

## Open questions

- **Window length.** 7 days is the decision; `config` does not expose it.
  Revisit if people ask for a different default.
- **Codex subagents.** The import brings in the parent; children stay
  `child_history_pending` until native child capture
  ([#355](https://github.com/wangjohn/agent-archive/pull/355)) lands, as
  with `backfill` today.
- **Repositories in temporary folders** (separate from this plan). Plain
  `backfill` treats a Git checkout under `/private/tmp` as a repository
  project (rule 4 runs before rule 6) and proposes adding it, e.g. a
  throwaway PR-review clone. Setup's import is limited to included roots,
  so it is unaffected, but `backfill` may want rule 6 first.

## Deviations

From the §1–§3 implementation (#386):

- **One plan, narrowed.** Setup builds one plan with no `Since` (the plan
  `offerSetupImport` built, so no new cost) and `backfill.Plan.Recent`
  narrows it: sessions that start before the day `--since 7d` names are
  marked `filtered_out` and counted as older, and the narrowed plan records
  `since`/`since_arg: 7d` in its filters, so its import record reads as
  `backfill --since 7d` in `backfill history`. For a session that is
  imported, the undated and dated plans decide the same way (both run the
  adapter over it); they differ only in which already-skipped sessions are
  read for a start time.
- **No projects, by construction and by check.** The filter is the included
  roots, and `Plan.Recent` also marks `filtered_out` any session whose root
  is not a configured, included project (`!ProjectIncluded`), so a recovery
  proposal or folder rule that would add a root is dropped. `ApplyToConfig`
  therefore adds no project, kept-out folder, or app (tested). Recovered
  sessions whose root is already included are kept, with the recovery policy
  bound again for the narrowed plan.
- **Shared commit path.** `importPlanLocked` is split: `registerImportLocked`
  is steps 4 and 5 (commit and register, then mark complete) and is what
  setup calls; `backfill --background` prints its usual lines around the
  same call. Setup prints only its own line.
- **Placement.** The import runs right after "Setup complete", before the
  skills and next-steps lines, so the result is next to the completion.
- **Failure text.** A registration error or Ctrl-C during registration
  prints only the cause (`simulated crash`, `stopped`) before "Run agent-archive
  backfill --since 7d to retry." A registration that stops part way leaves
  an incomplete import; the retry command (different options) starts a new
  import for the rest, and `backfill` names the interrupted one. Ctrl-C while
  planning prints "Stopped looking for recent sessions. Setup is complete.
  Import them later with agent-archive backfill --since 7d."
- **Older sessions alone.** When nothing is recent but older sessions exist,
  setup prints only the older line.
- **Review text.** The review's "! History import is separate." became
  "! Setup imports the last 7 days of included projects; older history is
  separate.", and the Codex details row "History" says "Last 7 days of
  included projects imported at setup; older sessions require backfill", so
  the consent screen says what setup does.
- **Next steps.** "Sessions already open are not captured" went from both
  forms of the line. The Codex all-projects form keeps its scope hint:
  "Start a supported new Codex task in any non-excluded project." when
  discovery is off (with discovery on, the Codex line already names the
  scope), and "Other apps still require an included project." with more
  than one app. In Codex all-projects scope, a session already open in a
  project that is not explicitly included is neither imported (the import
  is limited to included roots) nor captured; nothing now says so.
- **Pairing.** `machines pair` on the receiving machine finishes through
  `finishSetup` too, so it imports the last 7 days as well.
- **What keeps an imported session current.** The collector, not discovery.
  Every pass scans every accepted registration and rereads a transcript whose
  size or modification time changed, so an imported session's later turns are
  published like a hook session's (no hook, request, or second import needed).
  Discovery only recognizes the session: its `!authorized` branch lets the
  existing archive ID through, and `mergeContinuation` then leaves an
  import-origin registration as it is (it never moves an import's locator).
  Tested in `setup_recent_import_continue_test.go`, for Claude Code
  through setup and for a Codex session refused as `start_not_authorized`
  before `backfill --since 7d` imports it; refusing it in discovery's
  `!authorized` branch as well leaves the Codex test passing.
