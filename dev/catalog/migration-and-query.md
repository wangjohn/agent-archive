# Catalog-v4 migration and reads

Catalog-v4 is an opt-in destination format. Production S3 and R2 remain refused
until reviewed live conditional-write, same-version timestamp and provider-clock
qualification exists. Migration also requires a provider authority proving old
write credentials revoked, the source read-only, and every writer on protocol 10.
An operator flag cannot substitute for this evidence.

`agent-archive migrate --format catalog-v4 --prefix NEW_PREFIX` copies into an
empty, isolated destination. Use `--bucket NEW_BUCKET` when the original prefix
is empty or the prefixes overlap. Equivalent R2 endpoint/account spellings do
not create isolation. The command checkpoints a bounded source page only after
all its metadata, active source and preserved history bytes are hash verified.
Rerunning the same command resumes its durable checkpoint. Candidate archives
remain unavailable to ordinary readers until the exhaustive source/catalog
oracle and credential proof pass, followed by the local configuration cutover.
Original capture and preserved-history dates are unchanged.

`--rollback` restores the retained read-only source configuration and pauses
collection. It refuses if the activated catalog has subsequently changed.
Unresolved publication owners, mixed legacy metadata and missing/corrupt bytes
keep migration, collection and activation closed. Crashed owners never age into
permission.

Snapshots start their ten-minute monotonic lifetime when the request context is
created. Selected full metadata uses the pinned immutable reference and hash;
linked children and source resolution share that view. A changed or expired
continuation returns an explicit refresh error. A refreshed browser selection
uses a fresh request view.

| Query | Tree work | Discovery |
| --- | --- | --- |
| Exact canonical session identity | O(log N) | No canonical LIST |
| Global capture/activity root list, ordinary or replay, no dates | O(log N + returned rows) | No canonical LIST |
| All-session capture range including replays | O(log N + returned rows) | No canonical LIST |
| Direct project-tree range | O(log N + returned rows) | No canonical LIST |
| Scope tiers, title/text, ID prefix, complex predicates, date-filtered root child counts | Complete summary fallback | No canonical LIST |

Ordinary/replay immediate child counters use the existing harness+parent key.
Writers derive them in the same CAS as child changes; the full metadata body and
its revision stay authoritative. Counter-only leaf changes invalidate local
summary generations. Unknown prior roots rebuild complete summaries. A partial
range never becomes complete canonical listing evidence.

The private summary cache checks the entire persisted row tuple before reuse.
Immutable node/body cache reads verify the content hash and exact revision.
Missing, corrupt, canceled or root-inconsistent reads fail closed.

Maintenance uses the durable global admission coordinator plus any supplied
complete external history barrier. `_catalog recover --owner OWNER` requires the
recorded lease/inventory/head witness and only releases proven ownership.
`_catalog recover-seal --owner OWNER --generation GENERATION` is the separate
explicit recovery for a drained, unheld seal that never acquired a GC link.
Neither path accepts timeout, force or incomplete inventory as proof.

`catalog.WithReadView` preserves an existing request. Interactive browsers call
`catalog.NewReadView` for each newly selected session; this preserves cancellation
and deadlines while capturing a new head. It cannot extend an older view or its
cursors. Within one selection, metadata, linked children and transcript source
resolution share the captured root.

Show candidate projections carry explicit body provenance. A selected projection
loads authoritative immutable metadata. When this request already verified that
body during local summary hydration, `ReadCachedMetadata` may reuse only the
selected cached body after checking the bound complete generation, persisted row
tuple, exact revision, content hash and decoded identity. Missing or damaged body
cache entries fall back to the pinned immutable read; stale generation or damaged
row authority fails closed. Candidate bookkeeping retains verified keys rather
than every full body.

## Large metadata and writer version

The catalog uses writer protocol 10 and the exact `catalog-v4-v10` configuration
fence. Protocol numbers describe the wire format, independently of PR numbering.
Heads, admission records, pending journals and migration checkpoints reject older
protocol 9 state before admission. This format is opt-in and is not deployed here.

Tree nodes remain bounded to 128 KiB. Each serialized leaf, including its key and
record/count/revision wrappers, is measured at every update. Entries larger than
64 KiB use a strict `metadata-v10` envelope containing only index discriminators,
counters, revision and the existing immutable metadata reference. Full metadata
is never truncated or stored in a second payload namespace. Bounded queries stay
lazy and trim their selected rows before loading overflow bodies. Selected body
reads compare the exact hashed body against the envelope; complete fallback,
summary reconciliation, migration verification and GC explicitly resolve the body
and its full source/history references. Verified raw bytes may enter the existing
private metadata cache and are then discarded. Opaque resolved-row provenance is
bound to the exact snapshot and request lifetime and refuses caller mutation.

The 32 MiB metadata-body bound does not remove index-key constraints: canonical
session keys remain at most1024 bytes and encoded index keys at most4096 bytes.
Oversized project/parent/identity fields fail before head publication; no index
field is silently truncated or hashed into a different layout. Large linked-session
arrays are supported through overflow. A no-cache title/PR search still uses the
complete summary fallback and may read an oversized body again for the selected
full session. This O(N) fallback is complete and is not advertised as bounded or
as universally requiring only one body read.

## Interrupted collector publication

Configured destinations bind recovery to the existing credential-free destination
identity and the actual held collector flock. A frozen journal's verified recovery
descriptor is durable before admission. The destination independently verifies
the originating pending journal and exact body/source/history/predecessor evidence.
Only an identical owner may transfer across a restart or drain after a seal.
Parallel and stale invocations refuse; lock release joins guarded work.

Completion keeps bounded durable receipts protecting the body and all source and
history refs. Local acknowledgment and an explicit durable removal record precede
pending unlink. Receipt acknowledgment requires that exact record and completed
unlink; missing files alone prove nothing. Completion replay has no write authority.
Unresolved receipts are never evicted to make room. Direct unfinished owners with
no originating journal remain conservatively fenced for operator recovery.

Migration initialization and rollback durably bind their chosen owner and exact
source/destination/proof/head descriptor before sealing. Rerunning reconciles
only that recorded intent, including an interrupted first checkpoint; unrelated
coordinators and mixed objects refuse. Rollback retains its writer seal and
read-only mode. A completed unchanged-root rollback can finish local configuration
restoration again after a local save failure.

An admitted collector journal remains immutable. Missing/corrupt recorded source
bytes retain the journal and owner; restoring verified bytes permits exact guarded
retry. A later privacy/skill/filter policy, replacement request or clock cap that
would change the frozen digest refuses with the original journal intact. Those
policy conflicts intentionally remain operator-fenced until a supported exact
owner disposition exists; collection never republishes broader evidence just to
clear an owner. Only positively completed work with its originating held guard
and durable exact completion tombstone can be unlinked. Legacy replacement and
source-unavailable skip behavior remain available to legacy journals.

Remote-backed SQLite queries retain the captured request lifetime. Initial and
completed queries validate that lifetime; cursor continuations observe exactly
one fresh head version and reject a changed root. `Snapshot.ValidateContinuation`
does not renew the request or expose cursor authority. Local SQL cursors also bind
the captured root and a private refresh nonce, so a refreshed handle cannot reuse
an earlier cursor even when the root is unchanged.

Remote summary reconciliation runs the existing bounded64-directory cache
maintenance once per cache. Only a committed, complete verified delta or rebuild
can remove body cache directories: absence from the complete current identity universe. Each complete-summary
refresh inventories cache directory names in O(N), then retries best-effort
removal only for known keys absent from its verified complete universe. This
retries interrupted cleanup without adding a journal. Bounded list selection is
unchanged; selected ranges never prove deletion.
