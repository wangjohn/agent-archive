# Release remediation implementation plan

Status: implementation in progress; independent review/CI and disposable live
acceptance gates remain. Integration is pending. Prepared: 2026-10-01. Refreshed baseline:
`61d068b` on `main`, compared with `v0.1.1`. The original investigation used
`d85766f`; its observations below are historical reproductions, not claims
that every failure remains on the refreshed baseline.

## Refreshed execution record

- A: resumable cleanup is implemented in [PR #261](https://github.com/wangjohn/agent-archive/pull/261) and remains under independent review. The
  manifest remains immutable; recovery builds a new reviewable plan from
  original surviving keys. Integration and live disposable-provider acceptance
  are pending.
- B: the refreshed baseline already retains immutable rendered pager bytes.
  The remaining change tests complete fallback for startup failures, including
  shell exits 126/127 ([PR #259](https://github.com/wangjohn/agent-archive/pull/259)).
  Ordinary pager exit, signals, and Ctrl-C must preserve
  current behavior without replaying the listing.
- C/D: [PR #264](https://github.com/wangjohn/agent-archive/pull/264) makes the README reach app-specific verified
  capture and uses `list --all-projects --json --limit 0` for sizing. Live
  per-app onboarding acceptance remains pending.
- E: the baseline already uses a bounded durable admission-intent queue and
  moves staging disk synchronization outside its short queue lock. The
  remaining implementation in [PR #262](https://github.com/wangjohn/agent-archive/pull/262) addresses a pause/resume generation race so delayed
  intents cannot cross that boundary. Review, integration, final contention
  results, and latency evidence remain pending.

The package designs below retain the original requirements, adjusted where the
refreshed implementation changed their premise. No completed review, merge, or
final integrated test run is asserted by this record.

## Outcome and scope

Fix the four current review findings and investigate and resolve the existing
Cursor hook contention failure before the next release is signed off. Deliver
small, separately reviewable changes with reproductions and acceptance evidence.
Do not include the earlier, now-absent uncommitted prompt or source-installer
changes. Recheck the working tree and baseline before starting each package.

| Package | Problem | Priority | Suggested change boundary |
| --- | --- | --- | --- |
| A | Retried machine/filter cleanup leaves transcript objects after metadata was deleted. | P1 | Cleanup recipes, tests, and recovery documentation. |
| B | Pager consumption empties its fallback buffer. | P2 | Pager implementation and regression tests. |
| C | README quickstart omits per-app completion and verified capture. | P2 | README and first-capture guidance. |
| D | FAQ size calculation can use the newly capped session count. | P2 | FAQ calculation and unlimited-list example. |
| E | Overlapping Cursor hooks can exceed the registration lock deadline. | Release investigation | Capture, state/locking as needed, diagnostics, and meaningful contention tests. |

The two-second timeout installed in app hook files remains a constraint. Keep
hook execution nonblocking from the app's perspective, preserve admission and
privacy rules, and preserve registration/index consistency. This plan does not
add search, encryption, or another storage backend.

## Review evidence

- A: in both bash and zsh, fail the second deletion in a per-machine plan,
  after its metadata deletion. The prescribed fresh-plan retry succeeds but
  leaves three source objects for that session.
- B: a pager that consumes stdin and then returns an error produces empty
  fallback stdout while reporting that it is printing directly. A temporary
  Go overlay test reproduced this against the actual implementation.
- C: the README says a new session after setup is sufficient. The detailed
  guides correctly require Codex hook approval and an `archived, verified` row.
- D: `list` defaults to 50; the FAQ divides total object size by the session
  count from that command without specifying an unlimited or total count.
- E: `TestCursorOverlappingHooksRegisterOnce` failed in the race suite and in
  focused reruns with `capture registration busy; this hook was not recorded`.
  Its implementation is unchanged since `v0.1.1`; this is not established as
  a regression from the reviewed diff.
- Python installer/signing/purge suites and vet passed. Current-tree focused
  CLI tests passed. The broad race run was not green: capture failed and CLI,
  collector, and retention reached the ten-minute timeout. That run overlapped
  a working-tree change; use a stable final commit for release evidence.

## A. Resumable cleanup without orphaned transcripts

Files: [privacy recipes](../../docs/security/privacy.md),
[uninstall instructions](../../docs/getting-started/uninstall.md), and
[purge tests](../../scripts/test_purge_recipe.py).

### Design

Keep the existing prepare, inspect, apply workflow and zero-deletion behavior
on failed validation. Preserve metadata-first deletion; deleting sources first
would leave metadata pointing at missing content. Recovery must use the original
reviewed manifest, not rediscover ownership from metadata that may be gone.

1. Give every successfully prepared plan an immutable manifest recording its
   format version, bucket, prefix, mode, selector, creation time, selected
   sessions, exact target keys, and metadata snapshots. Record nonsecret
   provider/endpoint identity and bind recovery to the same destination. The
   existing temporary directory can be the plan location; print its absolute
   path so users can keep it through recovery. Use private directory/file
   permissions. Never save credentials or transcript content.
2. Keep attempt progress separate from that manifest. Before deletion, finish
   the existing listing and metadata rechecks. Record remaining/unconfirmed
   keys and actual errors, including interrupted execution. Failure to write
   required progress state must stop deletion.
3. Add a documented shell helper, `purge_resume PLAN_DIRECTORY`. This is a
   recipe helper, not a new agent-archive CLI subcommand. It validates the old
   manifest and destination, performs a fresh complete listing and preflight,
   and creates a new reviewable plan consisting only of original target keys
   that still exist. Successful deletions need not be inferred from logs.
4. For every remaining session, recheck any surviving metadata against the
   original snapshot. If metadata reappears or changes, or new objects appear
   within a targeted session, abort that recovery and explain the conflict.
   A missing metadata object from the interrupted plan must not make its
   original source keys ineligible for recovery. Do not absorb new keys or
   perform a broad unreferenced-source purge as an implicit fallback.
5. Applying the resumed plan requires explicit review again and a new
   five-minute validity window. Each apply attempt remains single-use. Resume
   must work across a fresh shell when the user supplies the manifest path.
6. Update partial-failure instructions to name the recovery helper, retained
   manifest, and exact remaining keys. All writers must stay paused through
   preparation, deletion, and recovery. Preserve the existing warning that
   shell recipes cannot guarantee atomicity against external writers and that
   versioned S3 buckets retain noncurrent versions. Explain manifest cleanup
   after success and that deleting the local manifest removes resumability.

### Regression tests and acceptance

Extend the existing fake-bucket tests in bash and zsh, for nested and empty
prefixes and for machine, old-filter, and all modes:

- Inject failure before and after each deletion, including after metadata is
  removed; resume and prove all originally selected objects are gone.
- Simulate a crash after a successful remote delete but before progress logging.
  Recovery uses listing plus the original manifest and reaches the same result.
- Retry more than once, and recover from a new shell. Already deleted keys are
  harmless; unrelated sessions and objects always remain.
- Reject unreadable/corrupt manifests, wrong destinations, changed/recreated
  metadata, incomplete listings, and new objects in a targeted session before
  the first recovery deletion. Do not silently widen the scope.
- An expired apply plan requires a fresh reviewed recovery plan, rather than
  bypassing validation. Simulate progress-write failure and partial failures.

Accept when those tests pass and a disposable live bucket failure-and-resume
exercise confirms that zero targeted objects remain. A successful exit and an
empty ordinary session listing are not sufficient proof of deletion.

## B. Preserve complete listing output for pager fallback

Files: [pager](../../internal/cli/pager.go) and
[pager tests](../../internal/cli/pager_test.go).

1. Preserve the baseline's complete rendered byte slice and fresh pager reader;
   never obtain fallback bytes from a consumed reader or buffer.
2. Preserve pager selection, JSON bypass, non-terminal bypass, disabling
   flags/environment variables, and startup-failure warnings.
3. Test startup failures before reading, after a partial read, and after a
   complete read; assert exact fallback bytes and the warning. Include real
   shell startup failures 126/127 in isolated CLI facilities.
4. Normal pager exits, ordinary nonzero exits, termination signals, and Ctrl-C
   must not replay output. A direct fallback stdout failure must propagate.

Accept when startup failures print the full original listing and current
list/JSON/pager behavior tests pass. No JSON or bucket schema change is needed.

## C. Make the main quickstart reach verified capture

Files: [README](../../README.md); cross-check
[first successful capture](../../docs/README.md#first-successful-capture),
[setup](../../docs/getting-started/setup.md#after-setup), and
[troubleshooting](../../docs/guides/troubleshooting.md#reading-status).

Replace the unconditional post-setup success claim with a short sequence:

1. Follow setup's final per-app instructions; in Codex approve the hooks with
   `/hooks` before starting the new session.
2. Start a new session/chat in an included project and send a prompt. Existing
   sessions do not establish a fresh capture.
3. Wait for the collector or run `agent-archive sync`, then inspect the app's
   `archived, verified` Capture row with `agent-archive status`.
4. Find the session with `agent-archive list` and inspect its metadata with
   `agent-archive show SESSION_ID`. Link directly to first-capture troubleshooting.

Keep the README concise, but place a privacy link before enabling capture and
briefly disclose best-effort redaction and lack of client-side encryption. Do
not equate overall Ready, valid hooks, or a storage connection with publication.
Do not promise immediate upload. Keep per-app details consistent with setup's
actual next-step output. Refresh the pinned install example when the release tag
is known; do not invent an unpublished tag in the executable quickstart.

Accept when the relative-link, documented-command, and CLI-reference checks
pass and a fresh user follows the README to verified capture in a disposable
account. Live app coverage is recorded individually; unavailable apps remain
pending rather than implicitly validated.

## D. Use the archive's total session count in sizing guidance

Files: [FAQ](../../docs/guides/faq.md#faq); cross-check
[list guide](../../docs/guides/list-and-show.md) and
[JSON contract](../../docs/reference/json-output.md#list---json).

1. Say explicitly that default text and JSON listings return at most 50
   sessions. For a full archive list, use
   `agent-archive list --all-projects --limit 0`.
2. In the sizing example, use `agent-archive list --all-projects --json --limit 0`'s
   `total_matched` when `total_matched_known` is true, with no narrowing
   filters. The refreshed schema is version 4; capped reads may omit the total.
   Do not use `returned` or `sessions.length` from
   a capped listing. Explain that the CLI count and byte listing must refer to
   the same bucket and prefix.
3. Handle zero sessions and unreadable/skipped metadata: call the result an
   approximate current archive size per readable session, not provider billing
   or an exact transcript size. Bucket object totals can include superseded
   objects and setup artifacts; versioned storage charges are a separate issue.
4. Include a small worked example: 200 MiB and 200 sessions gives approximately
   1 MiB/session even when only 50 rows are returned. Use an empty-archive case
   without division by zero. Avoid claiming a typical monthly cost.

Accept when the example uses the correct total for more than 50 sessions and
all quoted commands and links are valid. This is a documentation correction;
do not add an implementation-mirroring unit test for arithmetic prose.

## E. Resolve hook contention within the existing execution budget

Primary files: [hook runtime](../../internal/capture/hook.go),
[Cursor overlap test](../../internal/capture/hook_cursor_test.go),
[registration race tests](../../internal/capture/hook_registration_race_test.go),
[local locking](../../internal/local/files.go),
[state store](../../internal/state/store.go), and
[hook command](../../internal/cli/hook_command.go).

### E1. Establish a deterministic reproduction and root cause

- Reproduce the focused overlapping-hook failure on a stable checkout, first
  without unrelated package tests or compiler jobs, then under controlled load.
  Record OS, architecture, Go version, command, and sanitized results.
- Measure lock waiting separately from time holding `hooks.lock`. Count/trace
  registration, index, request, diagnostic, and filesystem synchronization
  operations inside the critical section. Keep temporary instrumentation out
  of release output and do not record raw hook payloads or transcript content.
- Build deterministic contention tests with controlled lock acquisition and
  injected I/O delays. Coordinate with channels/barriers rather than relying
  on random scheduling or large sleeps. Use any necessary narrowly scoped
  internal test seam; do not add a public timing/configuration API.
- Determine whether the bottleneck is avoidable global serialization, redundant
  durable writes, per-session request locks, or platform I/O. The current
  one-second lock wait is not the hook's total runtime budget: installed
  handlers have a two-second timeout including startup and all subsequent work.

### E2. Implement the smallest correct fix, with an explicit fallback decision

Preferred fix: reduce the globally serialized work and redundant writes while
preserving durable registration/index publication, per-session synchronization,
and the documented setup/retention lock order. Keep globally protected identity
allocation and configuration checks atomic; move only independent work outside
that lock. Do not remove fsync or weaken atomic writes simply to make timing tests
pass. Do not just lengthen the lock wait or the installed handler timeout.

If measured unavoidable contention still cannot meet the budget, this package
must implement a bounded durable admission inbox rather than call a timeout
increase a fix. Before coding that fallback, add a short design subsection to
this plan with the event format, lock order, replay ownership, and migration:

- Only an allowlisted minimal event representation may be persisted; never raw
  hook JSON, prompts, tool results, secrets, or extra unknown fields.
- Check inclusion and admission before persisting paths/identifiers. Preserve
  original observation time and start proof, and bind events to the applicable
  configuration/destination generation. Revalidate on replay; excluded,
  pre-activation, paused, setup-window, and removed sessions must not be newly
  admitted because processing was delayed.
- Atomically persist once, process idempotently before collector publication,
  and acknowledge only after registration/request state is durable. A crash at
  any boundary must not lose an accepted event or create duplicate registrations.
- Bound disk usage and replay work. Report a content-free actionable diagnostic
  for an event that cannot be retained or replayed. Account for cleanup,
  uninstall, legacy installs, and status visibility.

The refreshed baseline has implemented the admission-intent inbox. Retain the
requirements above as the design and regression checklist, and audit delayed
publication across pause/resume generations. E stays incomplete until the
remaining boundary fix satisfies the acceptance criteria. Ordinary lock exhaustion
currently only reaches stderr; ensure unrecoverable contention is visible in
status using the existing content-free, bounded diagnostic conventions.

### Regression tests and acceptance

- Concurrent Cursor first-prompt/response/stop events create exactly one
  registration, a consistent native-to-archive index, the correct first-event
  time, and the final valid transcript path/request state across controlled
  arrival orders. Test independent chats as well as one shared chat.
- Start proof for Claude Code and Codex remains required; a timeout/replay must
  not manufacture a fresh start. Setup, pause, exclusion, and destination-change
  boundaries remain enforced.
- Existing concurrent registration/forget and request/retention tests continue
  to pass. No orphan request or unreachable registration may be produced.
- Exercise delays and contention below, at, and beyond the supported budget.
  The normal burst must be retained and eventually publish; beyond-budget or
  storage-failure cases must be bounded and diagnosable without blocking the app.
- If an inbox is used, inject crashes at enqueue, processing, durable state
  commit, and acknowledgement, then assert idempotent restart and publication.
- Measure total `_hook` duration through the isolated CLI harness, including
  process startup, and leave documented headroom under the installed timeout.
  Wall-clock experiments supplement deterministic correctness tests.

Accept when the original overlap test passes repeatedly on macOS under race,
the deterministic contention cases pass, registration invariants hold, and
latency/recovery evidence justifies the chosen design. A diagnostic alone or
making a flaky assertion less strict does not demonstrate reliable capture.

## Documentation verification evidence

On the refreshed baseline with the C/D edits, focused CLI checks
(`TestList|TestDocs|TestCLIReferenceIsCurrent`) and `go test ./internal/doclinks`
passed. An isolated, temporary 200-session fixture using the existing published
fixture and index rebuild verified the documented all-projects JSON commands:
default returned 50 with an unknown total; `--limit 0` returned 200 with an exact
`total_matched` of 200. The temporary fixture was removed after verification.
These checks do not establish live app or provider acceptance.

## Delivery order and review

1. Land the remaining B regression coverage first; preserve current pager exit semantics.
2. Implement A, with deletion-boundary fault injection before adjusting recovery
   instructions. Require a focused review of the manifest, scope, and failure paths.
3. Land C and D as separate commits or one documentation PR with both findings
   explicitly covered. They have no dependency on E's investigation.
4. Run E1 while the other packages are implemented; E2 should be a separate code
   PR with root-cause evidence, before/after behavior, and contention results.
5. Integrate on one stable commit, run final CI, then follow the published-release
   acceptance record with disposable accounts/buckets and synthetic content.

Each PR description should
state the trigger, resulting behavior, regression test, and any remaining limits.
Do not mark a finding fixed solely because its new test passed while another
required invariant or live acceptance check remains unresolved.

## Verification and release gate

Run focused tests after each package. On the integrated stable commit, run the
required checks in [testing](../contributing/testing.md), including:

```sh
python3 scripts/test_purge_recipe.py
python3 scripts/test_install.py
python3 scripts/test_release_signing.py
go test ./internal/cli -run 'TestList|TestResolvePagerCommand|TestScreens|TestDocs|TestCLIReferenceIsCurrent'
go test ./internal/doclinks
go test -race ./internal/capture -run '^TestCursorOverlappingHooksRegisterOnce$' -count=20
go test -race ./...
go vet ./...
AGENT_ARCHIVE_PERF=1 go test -count=1 -run 'StayFast|FiveMegabyte' ./internal/collector ./internal/archive
```

Also require the CI lint, deadcode, fuzz, and verification jobs. If local full
suite runs hit resource-related timeouts, record them and compare with clean
platform CI; do not silently skip tests or present a longer timeout as a product
fix. Add new A/B/E tests to normal suites, not an optional manual-only job.

Update CHANGELOG with cleanup recovery and pager behavior. Add a clear upgrade
note that current JSON schema version 4 and the default 50-session cap require existing
full-archive analytics to use `list --all-projects --json --limit 0`. Refresh pinned installation
examples when the final published tag exists.

Use [release acceptance](../maintainers/open-source-acceptance.md) and
[releasing](../maintainers/releasing.md) for signed asset verification, live
GitHub settings, narrow S3 permissions, per-app publication/read-back, upgrade,
and disposable S3/R2 cleanup recovery. Missing access or an unavailable platform
is a named pending gate, not evidence of success. Do not test against the real
user's archive, hooks, LaunchAgent, or credentials.

| Gate | Pass condition | Initial status |
| --- | --- | --- |
| A | Every deletion-boundary failure resumes to exact targeted cleanup; unrelated objects remain. | Pending |
| B | Pager startup failure after partial/full consumption retains the full fallback listing; ordinary exit/signals do not replay. | Pending |
| C | Main quickstart reaches an app-specific verified capture. | Pending |
| D | Sizing denominator uses total sessions, including archives larger than 50. | Pending |
| E | Contention tests and repeat macOS overlap runs pass with bounded hook latency and no admission/index regressions. | Pending |
| Integration | Stable final-commit required CI is green; changelog/migration/install docs match that commit. | Pending |
| Release acceptance | Signed asset, upgrade, actual-provider read-back and cleanup recovery evidence recorded; coverage gaps explicit. | Pending |

Release sign-off requires A through E and integration to pass. Publishing a
candidate to obtain signed-asset evidence is distinct from promoting it as
validated; complete applicable release acceptance before promotion.
