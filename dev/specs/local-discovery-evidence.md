# Local discovery evidence and release gates

The implementation is present but production producer support is empty.
Do not enable a producer based only on fixtures, an upstream field shape,
a local file, or a matching local thread-index row.

## Evidence checked 2026-10-01

Codex release `rust-v0.159.3`, commit
`01fc69f4026735edfdf6789820549727a4867b11`, uses native first
`task_started`/`turn_started` metadata with a nonempty root turn ID and native
start. The built-in migrated-history exporter instead begins with
`external-import-turn-1` and no root ID. Reject the FIRST imported start,
even when a later resumed native turn exists. Missing first-turn metadata
inside bounded reads remains incomplete, never fresh-start proof.

Public source:

- [Native turn producer](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/core/src/session/mod.rs#L2305).
- [Migrated turn producer](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/external-agent-migration/src/sessions/export.rs#L105).
- [Importer creates ordinary fresh metadata](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/app-server/src/external_agent_migration/session_importer.rs#L448).

Disposable released-binary probes with a synthetic local provider found native
CLI `cli`/`codex-tui`, exec `exec`/`codex_exec`, and a synthetic app-server
client `vscode`/`Codex Desktop`, all at 0.159.3 and `history_mode: paginated`
without history-base or parent markers. The app-server client is not a real
desktop acceptance run. Native turn timestamps are integer Unix seconds,
while header time carries milliseconds; timestamp checks must allow less than
one second of quantization. UUID-shaped turn IDs are a conservative accepted
shape, not a claim that every Codex request path requires a UUID.

A copied native rollout remains observationally equivalent. In a disposable
brand-new Codex home, initialization without creating a task reconstructed
`state_5.sqlite` from a copied synthetic rollout, including source, originator,
version, cwd and original created-at. Matching that database row therefore
cannot establish local execution. Migration ledger absence is also insufficient:
the importer persists the rollout before recording its ledger and logs ledger
failure without removing the rollout.

## Required before producer activation

- Prove a supportable local-origin contract that excludes fresh copied/remote
  histories, or explicitly resolve the proposal's stronger provenance requirement.
- Exercise actual released desktop and CLI tasks, worktrees, fork/child,
  migration without a ledger, copied old/fresh history, archived moves,
  custom homes and permission failures on disposable machines.
- Verify hooks-absent setup, discovery, filtering, publication and read-back
  without changing hook trust; verify forward-only upgrade/pause/destination
  consent and journal recovery on the release platform.
- Meet the proposal's two-warm-pass latency and admission-lock p99 target under
  concurrent hooks, and document cold-backlog convergence with 1k/10k/100k
  files. Directory cookies and bounded caches do not promise universal latency.

Synthetic tests verify machinery, security boundaries and recovery only.
The production registry must stay empty until this evidence is available.

## Synthetic scan measurements

Linux container, Go 1.27.1, AMD EPYC 9V74, synthetic flat active root plus
an incomplete archived file; measured with `-benchtime=1x`. No provider,
real user state or bucket was accessed. These measurements exercise the
private synthetic producer contract, not production enablement.

| Files | Cold coverage passes | Cold wall time | Header bytes | Warm burst admission passes | Warm burst delay |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 4 | 144 ms | 503,000 | 1 | 54 ms |
| 10,000 | 40 | 3,855 ms | 5,070,507 | 4 | 661 ms |
| 100,000 | 392 | 55,183 ms | 50,916,332 | 123 | 17,812 ms |

The 8,192-record cache avoids unchanged header reads at 1k (two probes for
the new and incomplete files), but its bounded eviction produces 256 warm
probes per pass at 10k/100k. The proposal's two-pass warm admission gate is
**not met** for these large flat roots. Per-pass work remains bounded;
background reconciliation converges without starving the archived root.
Further scheduling/cache design and dated-directory, concurrent-hook,
release-platform CPU/RSS and admission p99 measurements are required before
activation. This implementation deliberately leaves these acceptance gates
open rather than interpreting directory names or mtimes as eligibility.

## Identity integrity and compatibility

Every new namespaced identity first writes an authoritative identity journal
record, then its derived namespace index, then registration. Deleting both
derived indexes recovers the same ID from that journal; present corrupt or
conflicting authoritative records fail closed. Legacy registrations and pending
parent/child candidates migrate in bounded directory batches. Initial decoding
runs outside admission locks; each record is revalidated under hooks.lock
before a journal write. New starts defer into the existing durable hook intent
queue while migration is incomplete. The collector advances migration before
storage initialization even with discovery disabled; explicit backfill completes
it before admitting its confirmed batch. Removed identities retain tombstone
semantics. The journal and migration state are authoritative local state;
corruption requires repair instead of allocating replacement identities.

Before any namespace or journal write on configured machines, a durable
writer fence protects the whole new-state contract, including legacy configs
without discovery consent. It preserves effective skill policy and grants no
discovery intervals. Protected configuration encodes schema_version as a
version/writer object; published v0.1.0/v0.1.1 integer decoders reject that known
field type before any write. Their source ignores the skill marker, so an
enum-only regression cannot establish refusal by these actual published
releases. The retained skill marker also rejects later intermediate
enum-validating writers. Existing numeric version-2 configs migrate to the
durable object before identity writing; already fenced configs do not rewrite.
Disablement preserves both guards; hand editing them is not safe downgrade.
Released Darwin binary execution remains an outstanding acceptance check;
source/decoder regressions do not replace it.

## Integrated source and scheduling behavior

Historical import and native handoff retain their own policy while sharing
Codex metadata decoding through nativesessions. Discovery and collector use
context-aware verified transcriptio snapshots; discovery retains its strict
root-descriptor confinement, first-start and bounded header requirements.
First-task timestamp checks permit one second of negative quantization relative
to header creation. The CLI can idle indefinitely before the first task, so
elapsed time between those records has no upper bound. Both native session
start and first-task timestamps reject more than two minutes of future skew
against the scan clock. Session creation still governs consent; a new prompt
does not make a session created before consent eligible. Current date and adjacent UTC date
directories are bounded scheduling hints, with 64 header probes/1,024 entries
reserved for priority and the remaining budget for fair backlog. Header time,
identity, source support and authorization still decide eligibility. The flat
root measurements above remain failed universal warm-latency evidence.

Existing discovery registrations can follow a validated active/archive move
without a new generation granting earlier start eligibility. A current active
source is preferred over an archived source, and a missing locator can be
repaired. Native identity, original start/cwd, admission, destination and
provenance remain immutable; publication revalidates the selected snapshot.


## Optional settled-index scheduling

Discovery additionally tries `state_5.sqlite` in each persisted Codex home as
an optional scheduling hint. Two constant queries use the released creation
and update indexes, return at most 32 locators each, and refuse missing or
incompatible indexes requiring a temporary sort. A locator passes the same
approved-root, regular-file, bounded-header, native-start, project and source
support checks as enumeration. SQLite timestamps never authorize admission;
reconstructed/copied index rows remain insufficient local-origin evidence.

The read-only Go SQLite VFS has no native write path. It opens only the
expected DB through confined regular-file reads, with a 100 ms context and
4 MiB page-read limit per root, and does not copy raw databases or create
native side files. Database replacement/change and any WAL/SHM/journal before
or after reading discard the hints. Live WAL is deliberately an optional miss:
using an immutable ordinary SQLite open there would ignore fresh rows, while
ordinary live read-only SQLite can race into creating a native WAL file.
Missing, corrupt, locked, unsupported, relocated/custom SQLite homes and live
WAL retain fair filesystem enumeration. Hint success never marks reconciliation
complete; newest-only hints also cannot promise coverage of arbitrary bursts.

Scan health separately reports `index_queries`, `index_locators`, and
`index_bytes_read`; `bytes_read` remains rollout-header bytes. Both hint lanes
share the 64-header/1,024-entry priority allowance, preserving the remaining
filesystem budget. Synthetic indexed flat/same-day benchmarks are recorded
separately from the prior filesystem-only results. Live-WAL and missing-index
warm latency, historic-header cache eviction, actual released desktop and
release-platform acceptance remain open.

The initial repaired synthetic benchmark run (Go 1.27.1, ordinary-user Linux
container, one warm burst per case, settled indexed DB) measured:

| Shape | Existing files | Cold coverage passes | Fresh admission passes | Warm scan ms | Index page bytes | Header bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Flat | 1,000 | 4 | 1 | 33.10 | 36,964 | 505 |
| Same day | 1,000 | 4 | 1 | 42.26 | 36,964 | 513 |
| Flat | 10,000 | 40 | 1 | 172.28 | 45,156 | 129,536 |
| Same day | 10,000 | 43 | 1 | 214.67 | 49,252 | 109,996 |
| Flat | 100,000 | 394 | 1 | 167.19 | 49,252 | 129,792 |
| Same day | 100,000 | 578 | 1 | 143.73 | 49,252 | 131,840 |

Each case issued four bounded SQLite statements (two EXPLAIN and two locator
queries). These initial repair measurements preceded the final adapter routing
and delayed-first-task repair; they demonstrate the settled-index mechanism,
not release acceptance or a frozen final-head performance guarantee. Reported
page bytes count VFS reads, including repeated page reads, rather than logical
row size. Warm scans still reread bounded historical headers after cache
capacity eviction. Synthetic allocations were 16.74–125.08 MB per operation;
CPU/RSS and concurrent release-machine acceptance remain unmeasured here.
