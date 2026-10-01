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

Before any namespace or journal write on configured machines, the durable
writer marker protects the whole new-state contract, including legacy configs
without discovery consent. It preserves effective skill policy and grants no
discovery intervals. Older supported-enum writers refuse the marked config.
Disablement preserves the marker; hand editing it is not safe downgrade.

## Integrated source and scheduling behavior

Historical import and native handoff retain their own policy while sharing
Codex metadata decoding through nativesessions. Discovery and collector use
context-aware verified transcriptio snapshots; discovery retains its strict
root-descriptor confinement, first-start and bounded header requirements.
First-start timestamp checks permit one second of negative quantization and
at most two minutes after header creation. Current date and adjacent UTC date
directories are bounded scheduling hints, with 64 header probes/1,024 entries
reserved for priority and the remaining budget for fair backlog. Header time,
identity, source support and authorization still decide eligibility. The flat
root measurements above remain failed universal warm-latency evidence.

Existing discovery registrations can follow a validated active/archive move
without a new generation granting earlier start eligibility. A current active
source is preferred over an archived source, and a missing locator can be
repaired. Native identity, original start/cwd, admission, destination and
provenance remain immutable; publication revalidates the selected snapshot.
