# `agent-archive backfill` — implementation plan

Status: in progress. This plan turns the [backfill spec](agent-archive-backfill-spec.md)
into work packages that separate agents can build and review. The spec is the
source of truth for behaviour; this plan fixes ownership, contracts between
packages, order, and the bar for "done". Where they disagree, the spec wins and
this plan is corrected.

Open questions in the spec are resolved by their recommendations: home and
temporary directories are skipped by default, added projects capture new
sessions, and "never delete" retention is out of scope.

## How the work runs

- **Orchestrator** (the main session) owns this plan, branches, integration,
  and the final live acceptance. It does not write feature code.
- **Implementer**: one agent per work package, in its own git worktree, with a
  fresh context. It reads the spec, this plan, and the code it touches, then
  implements, tests, and commits on its package branch. It does not push.
- **Reviewer**: a separate agent with a fresh context and no stake in the code.
  It reviews one package's diff against the spec and this plan, runs the
  checks, and reports findings ranked by severity. It does not edit code.
- **Fix round**: the implementer receives the reviewer's findings, fixes them,
  and reports what it changed and what it declined, with reasons. A second
  review happens only if the first found correctness problems.
- **Integration**: the orchestrator merges each reviewed package into the local
  branch `backfill/integration`, runs the full suite, and starts dependent
  packages from there. Pushing and PRs happen only when the person approves.

Every package must leave `go vet ./...` and `go test -race ./...` passing.

## Branches

| Branch | From | Contains |
|---|---|---|
| `claude/session-backfill-command-ea25ca` | `main` | spec and this plan (PR #17) |
| `backfill/integration` | spec branch | merged, reviewed packages |
| `backfill/b1-admission` | spec branch | B1 |
| `backfill/b2-discovery` | spec branch | B2 |
| `backfill/b3-import` | integration after B1 and B2 | B3 |
| `backfill/b4-undo` | integration after B3 | B4 |

## Waves

| Wave | Packages | Runs in parallel because |
|---|---|---|
| 1 | B1 admission model, B2 discovery and dry-run | They own disjoint files. B2 codes against a small interface and B1's contract below; the orchestrator wires them together at integration. |
| 2 | B3 import | Needs B1's fields and B2's plan. |
| 3 | B4 undo | Needs B3's batch file and registration. |
| 4 | B5 docs and live acceptance | Orchestrator, with the local MinIO recipe. |
| later | B1b destination ID; B6–B9 Cursor database | Not needed for phase 1. |

## Contracts between packages

These names are fixed so parallel packages agree. A package that needs to
change one stops and reports back instead.

**B1 provides (package `archive`, file `types.go`):**

```go
type SessionOrigin string   // SessionOriginHook = "hook", SessionOriginImport = "import"
type StartedAtSource string // StartedAtSourceHook, ...Transcript, ...FileCreated, ...CursorComposer

// New SessionRegistration fields: AdmittedAt, Origin, StartedAtSource, ImportBatch.
func (r SessionRegistration) Admitted() time.Time
func (r SessionRegistration) Imported() bool // Origin == SessionOriginImport
```

**B1 provides (package `collector`, file `removal.go`):**

```go
type RemovalReason string // RemovalReasonRetention = "retention", RemovalReasonUndo = "undo"

type RemovalRecord struct {
	Harness string        `json:"app"`
	Reason  RemovalReason `json:"reason"`
	At      time.Time     `json:"at"`
}

// RecordRemoval writes forgotten/<sha256(harness + "\x00" + nativeSessionID)>.json.
func (s *LocalStore) RecordRemoval(harness, nativeSessionID string, reason RemovalReason, at time.Time) error
// Removal reports the record for a native session, if any.
func (s *LocalStore) Removal(harness, nativeSessionID string) (RemovalRecord, bool, error)
```

**B1 provides (package `config`):** `Config.ImportedHarnesses []string`, and
`AcceptSession` admitting an imported registration whose harness is in
`Harnesses` or `ImportedHarnesses`.

**B2 provides (package `backfill`):**

```go
// ArchiveState is what backfill needs to know about the local archive. The
// CLI implements it over collector.LocalStore and config.Config.
type ArchiveState interface {
	// Classify returns "" when the native session is not in the archive, or
	// one of the skip codes already_archived, registered_not_admitted,
	// removed_by_retention, removed_by_undo.
	Classify(harness, nativeSessionID string) (SkipReason, error)
}

func BuildPlan(ctx context.Context, env Environment, archive ArchiveState, cfg config.Config, filters Filters) (Plan, error)
```

`Plan` carries, per candidate: harness, native ID, transcript path, resolved
project root and kind, start time and `StartedAtSource`, size, subagent
transcript paths, and a skip reason or none. It is the input B3 registers from.
`Environment` injects the home directory, the file system reads, and the clock,
so tests run over a fake tree.

## B1 — Admission model

**Goal:** everything except the command. After B1, a registration with
`Origin: import` behaves as the spec says in every existing code path.

**Owns:** `internal/archive/types.go`, `internal/archive/views.go`,
`schemas/metadata.schema.json`, `internal/config/config.go`,
`internal/cli/hook.go`, `internal/cli/hook_subagent.go`,
`internal/cli/collect.go`, `internal/cli/status.go`,
`internal/cli/verification.go`, `internal/cli/setup_transaction.go`,
`internal/retention/`, `internal/collector/` (except new files B3 adds).

**Scope:**

1. The fields, typed strings, `Admitted()`, and `Imported()` above.
2. Hooks set `AdmittedAt = SessionStartedAt = now` and
   `Origin: SessionOriginHook` on new registrations. Continuations keep them.
3. Every site in the spec's use-site table moves to `Admitted()`:
   `AcceptSession` (activation and destination), retention's
   `CurrentDestination` in `collect.go`, and retention's ages at
   `retention.go:133` and `:149`.
4. `ImportedHarnesses` in `config.Config`. `AcceptSession` admits an import
   whose harness is in either list. Setup carries it over from committed
   state, the way it carries `DestinationSince` in `setup_transaction.go`; it
   is never taken from a draft.
5. Removal records (contract above). Retention's whole-session expiry writes
   one with reason `retention` after `ForgetIdleSession` succeeds. Uninstall
   `--delete-local-data` removes `forgotten/`.
6. Imports skip the skill observer (`skillObserver` in `collect.go`) and get
   no hook lifecycle evidence. `SubagentCandidate` gains `Origin`; the
   materialize step copies the parent's `AdmittedAt`, `Origin`, and
   `ImportBatch` to the child, and writes the `subagentstop` lifecycle
   evidence only for hook children.
7. Metadata: `origin`, `imported_at`, `started_at_source`, all omitted for hook
   sessions, plus the `imported_without_hook_evidence` capture gap for imports.
   Update `schemas/metadata.schema.json`. Hook metadata stays byte-identical;
   the parser version does not change.
8. Status and verification: imports never set an app's `HookObserved`,
   verification state, or move it off "waiting for first session". Read-back
   verification still runs on them. Add the "Imported" counts to `status` and
   `status --json` (`imported_sessions`, `imported_pending`, `last_import`;
   `last_import` may stay empty until B3 writes batches).
9. A guard test that fails if non-test code compares `SessionStartedAt` with
   `ActivatedAt` or `DestinationSince` outside `Admitted()`.

**Done when:** the spec's Admission and Retention tests, the hook-resume test,
the subagent inheritance test, the metadata byte-identity test, and the guard
test pass, alongside the existing suite.

## B2 — Discovery and dry-run

**Goal:** `agent-archive backfill --dry-run [--json]` prints the spec's plan.
It is read-only: no local or remote writes.

**Owns:** new `internal/backfill/` (discovery, resolution, plan, rendering
helpers), new `internal/cli/backfill.go`, the `backfill` case in
`internal/cli/cli.go`, and its entry in `internal/cli/help.go`.

**Scope:**

1. Discovery for Claude Code, Codex, and Cursor transcript files, and the
   Cursor database count, exactly as the spec's Discovery section says,
   including identity checks and start times.
2. Project resolution rules 1–8, reading `.git` files but never running
   `git`, over the injected `Environment`.
3. Running each app's adapter over the full transcript to catch `empty`,
   `unsafe_format`, and `too_large`, with `min(8, max(2, NumCPU/2))` workers
   and a 128 MiB cap on bytes being read at once. Keep counts, times, and
   sizes only.
4. Filters (`--harness`, `--project`, `--since`, `--until`,
   `--include-home`, `--include-temp`, `--include-removed`) and skip
   precedence as in the spec's table.
5. The plan renderer: the project table, "Not a repository" group, totals,
   "Not imported", the three "If you continue" bullets, and the retention
   deletion date. The prompt line is printed only by B3; `--dry-run` ends
   with "Dry run: nothing was changed."
6. `--dry-run --json`, with the keys the spec lists.
7. The CLI's `ArchiveState` implementation over `collector.LocalStore` and
   `config.Config`: `already_archived` when the native index has an accepted
   registration, `registered_not_admitted` when the registration exists but
   `AcceptSession` rejects it. The removal-record lookup is added at
   integration, once B1 lands; until then it returns no record.
8. Refusals: setup missing. `--dry-run` works while paused or while setup is
   pending. Without `--dry-run`, B2 prints "backfill import is not available
   yet" and exits 1; B3 replaces that.

**Done when:** the spec's Resolution tests and plan golden tests pass (default
plan, filtered plan, JSON, every skip reason), discovery is tested over a fake
tree for all three apps, and a manual `--dry-run` against a sandboxed copy of
real transcripts prints sensible counts.

## B3 — Import

**Goal:** `agent-archive backfill` imports, per the spec's Registration and
concurrency section.

**Owns:** `internal/backfill/` (commit, registration, batches), additions to
`internal/cli/backfill.go`, `collector.Options.Progress`, oldest-first
ordering in `collector.Run`, `list --imported` and `--hook-captured`, `show`
fields, `backfill history`.

**Scope:** storage check with `storage.VerifyAccess`; the `[y/N/edit]` prompt
with retention edit; `--yes` and the no-terminal refusal; the configuration
commit under `setup.lock` → `collector.lock` → `hooks.lock` with the
fingerprint check and clock assertions; batch files in `imports/`; batched
registration under `hooks.lock`; subagent candidates; the upload loop with a
progress bar and safe Ctrl-C; `--background`; `history`; `last_import` in
status.

**Done when:** the spec's Command tests pass, including the hook-during-
registration flock test, crash injection between steps, idempotency, and
"declining changes nothing".

## B4 — Undo

**Owns:** `internal/backfill/undo.go`, undo in `internal/cli/backfill.go`, and
moving `deleteWholeSession` out of `internal/retention` so both can call it.

**Scope and done when:** the spec's `history` and `undo` section and its Undo
tests.

## B5 — Docs and live acceptance (orchestrator)

Update the eligibility doc, install.md, the main spec's "Historical import"
line, and the implementation ledger. Run the end-to-end check from the spec's
Tests section with the local MinIO recipe and copies of real transcripts in a
sandboxed `HOME`.

## Review checklist

Reviewers check each package against these, in addition to the spec:

1. **Correctness against the spec.** Every behaviour in the package's scope,
   and nothing outside it.
2. **Silent failure paths.** Boundary checks, retention decisions, and
   skipped sessions must not drop data without a count or a test.
3. **Privacy.** No transcript path, native ID, or content in output, logs, or
   diagnostics.
4. **Concurrency.** Lock order, time held under `hooks.lock`, and races with
   hooks, the collector, and retention.
5. **Tests.** They exercise the behaviour, not the implementation, and would
   fail if the behaviour broke.
6. **Fit.** Naming, comment density, and idiom match the surrounding code.
   No speculative abstractions.
