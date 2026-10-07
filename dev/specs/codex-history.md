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

## Native child admission and reconciliation

The global native candidate stream projects the stable thread, root and immediate
parent from shared Codex metadata. It never creates Claude composite subagent IDs
or enumerates a native home for each parent. Child root absence remains unknown.
A validated source snapshot supplies original same-thread creation and the first
OWN task. Its timestamp must be no earlier than original native creation, with
the same one-second tolerance as bounded header admission. A rejected first
task cannot be replaced by a later native-looking event. Discovery, historical
import and ongoing capture apply that constraint. Earlier private native-child
scan proofs are revalidated once; parser, adapter and privacy versions remain
unchanged by this admission correction. The inherited prefix may
exceed the bounded catalog header window;
metadata-only identity/creation/format facts do not authorize capture.

Each native child carries its own origin, admission, destination and import batch.
Missing parent links do not prevent independently admissible capture. Relationship
repair uses the existing registration inventory, scoped to the same native source
home, project and destination; ambiguous owners and removal tombstones remain
unresolved. Parent hooks and admission are never substituted for child consent.
Native discovery and import do not fabricate SubagentStop observations.
Relationship repair queues its durable publication obligation before marking the
link reconciled. Resolving a missing parent preserves the child's capture age and
own usage. Privacy maintenance accepts an unresolved historical parent only for
positively identified native child evidence; conflicting known parents are refused.

Earlier writers could retain native children without the optional public ownership
marker. A supported persisted child binding for the exact already admitted owner
can migrate that marker before retained maintenance. All current and preserved
headers are upgraded under their original source authority; unknown or conflicting
owners stay refused. This migration reads no current native inventory and does
not change admission, capture age or raw ownership. Absent legacy public markers
remain compatible with readers.

`native_child` in retained source and metadata records ownership independently of
`parent_session_id`. A captured child with an unresolved archive parent includes
`native_parent_link_pending`. Source-history dependencies remain distinct from
these conversational relationships. Resolving a parent for an existing revision
set uses sequential retained maintenance to update every referenced source header
before the sidecar changes; exact reader identity checks remain in force. Each
frozen input retains its original parent header independently of the prepared
output target, so a policy successor can verify both old and prepared stages.
Older journals recover that input fact from checksum-matching predecessor
references, or retain their unchanged header authority. Parent deletion does not remove a child's
self-contained retained history. Undo uses exact batch membership and orders only
selected descendants before selected ancestors.

Initial discovery shares one serial native source pass for the active authorized
home. Switching homes closes the previous pass, preserving the aggregate 128 MiB
provider data charge. Actual source read calls/bytes/opens are counted separately
from bounded metadata probes; this is not an RSS or heap cap.

Legacy Codex composite links are reconsidered with one exact local index lookup
for `parent:subagent:child` when the independently admitted native child and parent
are known. An unadmitted matching reservation receives an existing linked-evidence
provenance marker, `native:legacy-unverified-composite`, and a content-free
`native_child_link_unverified` gap. Raw historical supplemental evidence stays
retained; current derived links exclude only that positively identified ghost.
Unknown local evidence and legitimate unavailable links stay explicit. A private
`native_link_version` registration marker makes this migration idempotent across
restart, without deleting identities or reading parent source bundles.
