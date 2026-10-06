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

`internal/rolloutcatalog` implements one lazy, serial observation epoch over
explicitly approved Codex homes. It exhaustively enumerates `sessions` and
nested `archived_sessions`, with separate root, directory, entry and header
budgets. Missing stores are observed as absent; unreadable stores, unknown
headers, cancellation and exhausted budgets cannot establish completeness.
Thread identity remains Codex-qualified and independent of the source home.
Creation, producer, relationship, cwd and recorded repository conflicts remain
unresolved. Physical copies coalesce only after identity and bounded observed
prefix agreement; lexical order only breaks ties between proven copies.

Settled `state_5.sqlite` files supply optional id-indexed current locators through
a confined read-only VFS and a bounded read transaction. Schema and indexed
query-plan probes precede lookup. Missing, stale, unfamiliar or live WAL/journal
state falls back to complete file-lineage evidence. SQLite paths must match
opened catalog identity beneath an approved store. No database snapshot or
sidecar is copied, and no native write is permitted.

Revision tokens bind candidates, header/prefix evidence and current selection.
`Check` revalidates all observed nested directory membership, file identities,
changed headers/prefixes and native locator state, within an aggregate budget.
Unchanged header evidence is shared across lookups; revalidation stats are counted
separately from enumeration. Single-source appends preserve header evidence;
appended duplicate copies require a renewed epoch before choosing a copy.
Filesystem observations use the same practical size/mtime/identity contract as
verified transcript snapshots; restored timestamps are not a filesystem lock.
Each dependency uses its independently approved root opener, including across
homes. Catalog metadata grants no authorization to retain ancestor content.

Ordinary collection leaves this catalog unopened. Only a validated pending
related-history refusal triggers a bounded metadata-only locator diagnostic,
using approved discovery homes and one shared pass catalog. This diagnostic
reads no duplicate transcript prefixes and establishes neither recoverability
nor permission to publish. Explicit source/filter callers can opt into the
existing lookup port; history admission and mutation remain fenced until the
later lifecycle and import stages are implemented.

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

Until lifecycle support installs publication journaling and writer protection,
collector capture, metadata refresh, privacy mutation, generation recovery and
retention/deletion refuse history artifacts. The temporary Codex publication
fence reads the previous remote metadata body once before a mutation, including
metadata-only refreshes and pending-publication retries. Unchanged admission
continues using the existing summary/signature path, without full published-state
decodes or per-session writes. Explicit generation recovery separately checks
local cached, published and metadata history before changing generation state.
The next lifecycle stage replaces these temporary fences with validated
source-set summaries, atomic publication and protected retention.

The initial same-handle `SourceAdmission` check binds native ID and cwd only.
Lifecycle integration must extend it with immutable creation, producer and
relationship facts needed by the original admission; these two fields alone
are not a complete ongoing authorization proof.

Producer references: [ordinal writer](https://raw.githubusercontent.com/openai/codex/rust-v0.160.0/codex-rs/rollout/src/ordinal.rs)
and [history materialization](https://raw.githubusercontent.com/openai/codex/rust-v0.160.0/codex-rs/thread-store/src/local/thread_history_materialization.rs).
Synthetic tests model the pinned producer contracts; no private transcript is a
fixture or an acceptance claim.
