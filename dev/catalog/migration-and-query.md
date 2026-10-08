# Catalog-v4 migration and reads

Catalog-v4 is an opt-in destination format. Production S3 and R2 remain refused
until reviewed live conditional-write, same-version timestamp and provider-clock
qualification exists. Migration also requires a provider authority proving old
write credentials revoked, the source read-only, and every writer on protocol9.
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
