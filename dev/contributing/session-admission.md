# Session admission

How a registration's times are used, for contributors. The rules users see
are in [session eligibility](../../docs/reference/session-eligibility.md).

`agent-archive backfill` registers sessions the hooks never saw, after the
person confirms a plan ([backfill design](../specs/backfill.md)). An
imported registration has `origin: import` and two times:

- `session_started_at` is the true start, from the transcript
  (`started_at_source: transcript`) or, for a Cursor file with no timestamps,
  the file's birth time (`file_created`).
- `admitted_at` is when backfill took ownership. A hook registration sets it
  to its own start.

Every boundary check uses `archive.SessionRegistration.Admitted()`
(`internal/archive/types.go`), which is `admitted_at`, or
`session_started_at` for a registration older than that field.
`AcceptSession` and `InCurrentDestination` are methods of `config.Config`
(`internal/config/config.go`):

| Check | Uses |
|---|---|
| Project activation in `AcceptSession` | `Admitted()` |
| Storage destination in `AcceptSession`, and retention's current-bucket check (`InCurrentDestination`) | The registration's `destination_id`, set at registration by hooks and backfill; `Admitted()` against `DestinationSince` for a registration without one |
| Retention age before a first capture | `Admitted()` |
| App selection | `Harnesses`, plus `ImportedHarnesses` for imports |
| Fresh-start eligibility for hooks, metadata `started_at`, subagent ordering, handoff | `session_started_at` |
| App hook observation | Durable `HookObservedAt` from an actual supported hook; independent of original registration origin |
| Discovery fresh-start permission | Native original creation within the current agent/project/destination authorization generation and its unpaused intervals |

Two guard tests in `internal/archive/admission_guard_test.go` read the
source. `TestNoBoundaryComparesSessionStartedAtOutsideAdmitted` fails if code
compares `session_started_at` with `ActivatedAt` or `DestinationSince`
outside `Admitted()`, and
`TestNoDestinationTimeComparisonOutsideInCurrentDestination` fails if code
compares an admission with `DestinationSince` outside
`InCurrentDestination`.

A hook that later resumes an imported session continues it: the
registration keeps its start, admission, destination ID, and origin.
Retention and undo leave a removal record when they forget a session, so
backfill does not import it again (unless `--include-removed`).

## Shared discovery foundations

Automatic discovery is not activated by these foundations. `sourcefacts` supplies shared project and worktree facts; those facts never
authorize capture. Strict source readers belong with the scanner integration.
`config.ReconcileDiscovery` records forward-only permission generations and
`DiscoveryGeneration` checks native creation against half-open intervals.

`state.RegisterOrMerge` uses the existing qualified SessionKey reservation and
request-lock protocol, preserves compatible original provenance, and rejects
removed automatic candidates. Explicit imports retain their removal override.
The older low-level `RegisterNewSession` remains a replacement primitive for
existing controlled callers; new admission paths must use register-or-merge.
A real hook records `HookObservedAt` without relabeling imports or discovery.
Generic hook locators cannot replace a discovery-owned confined source.

Protected authorization uses a schema-version object that published integer
decoders refuse. Legacy hook-only config remains numeric. Setup integration must
fence protected Before snapshots before journaling a reconfiguration; never
recover a numeric snapshot over protected authorization.
