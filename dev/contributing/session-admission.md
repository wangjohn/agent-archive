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

## Scheduled identity recovery

Recovery retains the qualified identity census as its authority. Each scheduled
slice validates the complete registration inventory and all duplicate owners,
then resumes derived-index application from a content-free cursor tied to that
inventory, the request generation, and the membership revision. A partial,
interrupted or damaged cursor never certifies absence. Candidate reservations
and requested misses must also finish before the complete marker is written.
Repeated requests for an already retained exact key preserve progress; a new
request changes the recovery generation.

Registration membership changes stage a small durable revision outside locks.
The request lock precedes the short membership lock, which commits the revision
before the registration rename. Completion takes hooks then membership and
checks the revision before committing the certificate. The membership lock
never acquires hooks/request locks or covers staging, syncing or a census.
Valid continuation updates retain identity and do not change the revision.
Recorded membership evidence that disappears stays uncertain rather than
becoming a legacy store again.

The scheduled application allowance is at most four seconds and starts after
complete registration validation and fingerprinting. One deadline charges all
application phases, including later phase inventories and checkpoints. The
caller context bounds the entire stage separately; if complete validation
cannot finish within it, recovery stays uncertified. The CLI reserves checkpoint
time and publication time within its existing soft/hard budgets. Complete
inventory reads and one in-flight atomic operation can exceed an allowance on
slow storage, so this is a measured scheduling target rather than an arbitrary
host IO deadline. Local recovery/discovery still precedes credentials/storage,
and publication of already admitted sessions can proceed while recovery is
pending. The CLI avoids a second recovery slice inside the collector. Synthetic fixtures exhaust the same scheduled engine within their contexts;
production consumers retain bounded scheduling.

Recovery health reads bounded, content-free marker/cursor/fence evidence and
reports pending phases separately from completed coverage; missing or corrupt
evidence is unknown. It never reads native sources, Git or storage.

A validated committed qualified index for the same key/archive owner is left
unchanged after the request-lock registration check. Recovery repairs corrupt,
conflicting, requested, absent or unfinished reservation entries instead of
using that optimization. Existing-index and missing-index migration benchmarks
are separate cases; their allocation and elapsed distributions are not pooled.
