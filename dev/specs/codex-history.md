# Codex retained history reader

A stable Codex thread is distinct from its physical rollout revisions, root
conversation, parent and fork relation. The source provider consumes injected
`CodexRolloutLookup` catalog evidence; it never enumerates a home directory per
thread. An ordinary-looking original rollout also consults the lookup when
available, because a later physical revision may be current.

A current locator is an untrusted hint. The opened header must match the stable
thread, the file must remain inside its admitted source root, and the lookup
revision must remain current. Without a current locator, selection requires a
complete candidate set with one connected, unique lineage tip. Missing or
ambiguous evidence remains pending. Modification time, file size and lexical
order never choose a current revision.

Physical `history_base` edges form an ordered graph. Each included prefix ends
on a newline and agrees with the raw exclusive ordinal boundary. Cycles,
missing or duplicate physical IDs, split records, ordinal gaps/regressions,
unknown history modes and contradictory bounds fail closed. Blank lines do not
consume ordinals; safely omitted unknown native records do. A valid JSON leaf
record without its final newline remains pending for the next capture.

Logical ownership is separate from the physical graph. An explicit child or
fork boundary excludes copied prefix activity. An absent child boundary excludes
nothing, matching the producer's optional-boundary behavior. A fork boundary
survives later same-thread reverts; same-thread history before a revert otherwise
remains own activity. The active header supplies identity and native start.
Inherited model context can inform own messages, but inherited prompts, tools,
usage, titles and timestamps do not become own activity. Cumulative-only token
counters with uncertain scope produce an explicit gap, without guessing whether
a child reset its counters. Independent own per-call usage remains useful.

## Shared native catalog

`discovery.CodexRolloutLookup` owns the pass's requested coverage, native current
projection and optional full metadata epoch. Ordinary collection and generation
preview retain their requested lane. `MetadataInventory` explicitly requests a
lazy view with separate bounds: 16 homes, 16384 physical entries, 2048 directories
and 16MiB aggregate metadata facts/header work, charged to the shared native read
budget. A late request starts a complete following round; neither observation
cache membership nor requested coverage proves complete inventory.
Physical candidates count before header validity; filesystem fingerprints include
subdirectories and unrelated entries and retain a separate18432-entry traversal
ceiling. Aliases of one canonical home share acquisition while every approved
home spelling remains fenced.

The full view enumerates approved active and archived stores with the same
bounded directory adapter. Each header is acquired on one confined descriptor
using one-byte ReadAt requests through the first newline, at most64KiB including
that delimiter. A resumable step performs at most1024 reads and100ms of work,
retaining one in-memory partial header. Cancellation or changed root/source
clears the cursor. It reads no body, task event or duplicate prefix. Missing
stores have absence evidence; unreadable stores, unknown/malformed headers,
identity conflicts and exhausted bounds leave incomplete evidence. Every
physical copy remains represented; complete enumeration does not establish
content agreement or ancestor capture permission.

Caller-owned validation slices share one filesystem sweep for up to512 lookup
and check calls and30seconds after the sweep, with a60second maximum. A sweep
rechecks captured directory membership stamps and every physical file
observation; selected providers independently check opened headers/content.
Thread and Check retain live targeted current-row queries and WAL refresh/replay
despite filesystem proof reuse. Ordinary requested work retains its five-second allowance. An explicitly
requested full epoch has one fixed thirty-second cumulative active-work
allowance for acquisition, sweeps and current checks. Caller cancellation and
earlier deadlines win; slice renewal and lifetime never replenish that allowance. Failed sweeps are retained only in their bounded slice; callers
close snapshots before renewal. CloseReadOnly releases resources without a
capture-catalog checkpoint. Filesystem observations use the practical
size/mtime/identity contract; restored timestamps are not a filesystem lock.

Only actual path-specific related-history refusal requests the same owner's
view for a content-free diagnostic under a100ms caller deadline. It preserves
the original error and says this operation remains pending. Upstream admitted
and retained history paths retain their existing behavior. No second production
catalog, new content authority or new history admission is introduced here.
The PR-only legacy prefix-coalescing catalog and settled-VFS engine are retired;
metadata completeness does not establish duplicate content agreement. Authorized
both-copy prefix checks and cross-home ancestor content remain separately owned.

## Captured-prefix consistency and resources

Each snapshot fixes a newline-complete prefix, validates its raw ordinals and
hashes its bytes. Before returning filtered output, the provider hashes that
same native prefix again and verifies both descriptor and locator identity.
Headers used for selection, graph edges, ownership and preliminary admission
are proven against their originally observed bytes, including dependencies
that contribute zero captured bytes. Those header proofs retain descriptor
leases and are checked before returning a snapshot, at admission and before
returning filtered output, for ordinary and related captures.
Appends beyond the captured prefix are allowed; replacements and prefix rewrites
are retryable changes. This permits continuously active chats to make progress.

A pass owns at most 64 open dependencies, depth 64 and one million records per
materialized graph. The shared 128 MiB charge includes captured input extents,
immutable ancestor-prefix byte caches, bounded prefix summaries and reserved
borrowed scanner buffers. One ancestor prefix of at most 32 MiB may be cached
per open physical file when the remaining budget permits it. Descendants reuse
its bytes and validated ordinal summary. The provider still reads and hashes
the native prefix once per completed descendant capture; it does not use file
stamps as proof that cached bytes remained unchanged. Changed dependencies evict
unused entries, while live snapshots prevent unsafe replacement. Close releases
leases, cached bytes and descriptors; cancellation/error closes the snapshot.

The byte charge is not a Go heap or RSS cap. Privacy filtering, encoded retained
records and assembled native maps consume additional memory. Shared prefixes
are filtered again for each self-contained output: the privacy codec has
stateful interpretation, so no unproved filtered-result reuse is introduced.
There is one bounded immutable raw copy, rather than one raw copy per child.
`BenchmarkRelatedHistoryRecords` measures actual native reads, opens, cache hits,
filtered/output records, allocation and sampled peak heap for 1k/10k/100k records
and four children sharing a 100k-record prefix.

## Reader-before-writer compatibility

Ordinary sources retain source schema 2, metadata schema 1 and their byte/record
behavior. History sources use schema 3: a bounded manifest plus exact unsigned
ordinals in native-record envelopes. The filtered native objects never carry
unvalidated metadata, absolute native locators or copied producer instructions.
Spans are recomputed after privacy drops, and retained ordinal precision exceeds
the floating-point native map range. Source schema 3 is Codex-only.

Metadata schema 2 adds an active physical revision and bounded preserved source
references. Every source key must be checksum-addressed under the same archive
session prefix. Prior source-schema-2 references are permitted. Reader validation
precedes derivation and selection; a malformed reference cannot widen reads.

The collector publishes related histories through its existing durable pending
journal. Every final reference is verified before exact predecessor preflight and
metadata replacement, and every reference plus exact metadata bytes is read back
before acknowledgement. An exact committed retry performs no source or metadata
PUT. Stronger privacy resolves predecessor/final state before replacing private
work; unknown authority remains pending. Older config writers remain protected by
the permanent config-v5 writer marker.

Retained maintenance carries the complete source set and original captures.
Frozen generations use retained bytes and recorded Git facts. Retention uses the
latest meaningful archive capture across the set, with the existing clock clamp;
parser, link, privacy and representation work do not manufacture activity. Cleanup
protects all live references, rechecks metadata before source deletion and removes
metadata before whole-prefix deletion. Unknown schemas and identities refuse.

Capture admission independently validates immutable native creation, producer,
relationship and project facts. A previously unknown native home can migrate only
through a configured confined root containing the admitted locator, rechecked
before binding persistence. Lookup hints never grant that permission. Read-only
source interpretation remains separate from capture authority.

Producer references: [ordinal writer](https://raw.githubusercontent.com/openai/codex/rust-v0.160.0/codex-rs/rollout/src/ordinal.rs)
and [history materialization](https://raw.githubusercontent.com/openai/codex/rust-v0.160.0/codex-rs/thread-store/src/local/thread_history_materialization.rs).
Synthetic tests model the pinned producer contracts; no private transcript is a
fixture or an acceptance claim.
