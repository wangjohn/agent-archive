# Codex session names

Collection can refresh `name` for already admitted ordinary Codex sessions.
`title` remains the first human prompt fallback. Listing reads archived metadata
and does not probe native storage or launch Codex.

The file resolver is pinned to Codex 0.159.2 default storage. It uses a pure
read-only SQLite VFS for settled `state_5.sqlite`, refusing any WAL, SHM or
journal. A matching row must carry the admitted rollout path and producer.
Paginated history uses `threads.name`; legacy history uses a distinct native
title or the last usable physical `session_index.jsonl` record, suppressing
the native preview fallback. Index-only resolution requires verified legacy
source context and absent default database. Unknown producers, relocated
SQLite storage, ambiguous homes, incomplete files and related histories are
unavailable. This support does not promise parity with a running desktop
sidebar. No native API process is launched.

Unavailable observations preserve the last verified name. Confirmed absence
clears it; an index-only miss cannot clear a previous canonical database name.
Rename publication uses retained source evidence when the native transcript is
unchanged or missing, preserving original activity, capture time and retention.
Attempted publication retries keep their frozen bytes; a newer rename is owed
after the retry succeeds.

Each pass budgets 64 admitted targets, 16 configured homes and two seconds.
Index scans are limited to 4 MiB and 32,768 lines; a database read is limited
to 100 ms and 8 MiB of reads per home. The persistent target cursor advances
over deferred targets, with bounded backoff for unavailable observations.
Lookup context is content-free and keyed to retained source checksum and codec
contract. Migration reads at most 8 MiB per retained JSON publication and
16 MiB per pass; subsequent unchanged lookup reads only a bounded leading
checksum and stat. These limits defer work without delaying normal capture.
Aggregate exhaustion is deferred to another pass. A retained publication over
the per-session 8 MiB ceiling remains unavailable for this rename-only path;
cursor rotation cannot make it fit. Its prior name and pending transaction
remain intact. Real native content changes still use normal capture limits
and may publish the latest resolved label alongside the new conversation.

Context cache entries store only IDs, filtered evidence, producer/contract,
hashes, timestamps and scheduling counters. A name-only change publishes a
new filtered source and metadata through normal durable pending publication,
including the listing revision. Capture permission, frozen generations and
retention policy continue to govern whether retained evidence can be used.
The lookup cache holds at most2,048 entries and4MiB of compact JSON. Eviction
does not remove a name already retained in published source evidence.
