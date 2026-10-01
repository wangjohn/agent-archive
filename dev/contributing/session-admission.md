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

## Discovery admission

`Config.DiscoveryGeneration` is the named fresh native-start guard. Only the
current agent/project/destination generation can admit new discovery sessions.
Unpaused intervals are half-open, and pauses preserve earlier intervals in the
same generation for delayed discovery. Reconfiguration, disable/re-enable,
project exclusion/reinclude and destination changes rotate forward-only scope.
Invalid timestamps have no file-time or scan-time fallback. The future skew
limit is two minutes. Publication still compares `Admitted()`, never native
start, with project/destination boundaries.

Setup journals configuration and permission history together. Pause/resume
commits both in one atomic config write under collector then hooks locks.
`RegisterOrMerge` namespaces native identity by canonical agent, validates
legacy ownership, preserves original origin/admission/start/destination, and
honors automatic-admission tombstones. A registration exists before a request;
collector registration scanning recovers a crash between these durable writes.
Actual `HookObservedAt` is distinct from origin; imported sessions alone and
discovery registrations alone do not establish hook execution. A later actual
hook on an import establishes observation while preserving import attribution
and leaving fresh automatic-capture verification unproven. Hook continuations
preserve discovery-managed locators; only validated source discovery handles
their active/archive replacement.
