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
| App hook verification, `HookObserved`, skill inventory | hook-registered sessions only |

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
