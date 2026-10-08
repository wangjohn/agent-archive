# Codex session names

Collection can refresh `name` for already admitted ordinary Codex sessions.
`title` remains the first human prompt fallback. Listing reads archived metadata
and does not probe native storage or launch Codex.

The file resolver is pinned to Codex 0.159.2 default storage. It uses a pure
read-only SQLite VFS for settled `state_5.sqlite`, refusing any WAL, SHM or
journal. A matching row must carry the admitted rollout path and producer.
Paginated history uses `threads.name`; legacy history uses a distinct native
title or the last usable physical `session_index.jsonl` record, suppressing
the native preview fallback. Index-only resolution additionally requires positive per-home proof of the
producing session’s default SQLite placement and an absent default database.
Production supplies no such historical placement proof, so index-only homes
remain unavailable; a settled matching legacy database may use the index fallback. Unknown producers, known relocated
SQLite storage, ambiguous homes, incomplete files and related histories are
unavailable. This support does not promise parity with a running desktop
sidebar. The default `files` mode launches no native process.

Storage guards observe the collector's current `CODEX_SQLITE_HOME` and the
native home's local configuration, but cannot prove historical, system or
runtime placement. Missing local SQLite and configuration files therefore do
not authorize an index-only canonical name. The optional provider port accepts
positive per-home producing-storage proof from a verified host; ordinary
collection leaves that proof empty. Settled matching database rows remain
available only with supported retained producer and exact projected SQLite
column/value types. Unknown producer, schema or value shape preserves prior
verified evidence.

External naming requires acknowledged ordinary source-schema-2 and
metadata-schema-1 authority. A history source set remains outside this lookup
even when its active source is ordinary; cached name debt cannot replace its
revision journal. A transition to supported history capture proceeds without
attaching an ordinary external name observation.

Unavailable observations preserve the last verified name. Confirmed absence
clears it; an index-only miss cannot clear a previous canonical database name.
Names whose normalized text cannot remain stable through privacy filtering
are also unavailable and preserve the last verified name.
Rename publication uses retained source evidence when the native transcript is
unchanged or missing, preserving original activity, capture time and retention.
Attempted publication retries keep their frozen bytes; a newer rename is owed
after the retry succeeds.

Each pass budgets 64 admitted targets, 16 configured homes and two seconds.
Index scans are limited to 4 MiB and 32,768 lines; a database read is limited
to 100 ms and 8 MiB of reads per home. Homes are visited in persistent target order. A separate durable priority
cursor skips the previous first work group across ID-order wraparound, so
interleaved IDs in a slow home cannot monopolize later homes’ priority. A bounded
per-group archive-ID cursor also rotates the first target within each prioritized
group, preserving deferred-ID opportunity after lookup backoff expires. Group
identities are hashed by the collector before persistence; removed targets
retire their cursor, while temporary backoff preserves progress. The target cursor advances
over deferred targets, with bounded backoff for unavailable observations.
Lookup context is content-free and keyed to retained source checksum and codec
contract. Migration reads at most 8 MiB per retained JSON publication and
16 MiB per pass; subsequent unchanged lookup reads only a bounded leading
checksum and stat. These limits defer work without delaying normal capture.
Serialized and decoded context also share the pass's data budget; a borrowed
publication remains charged until its pass scope ends.
Rename integrity verification and candidate encoding reserve compression scratch
and output against the same ledger; verification output is released before the
renamed candidate is rendered. Shared pressure preserves the last acknowledged
source and deferred name evidence for a later pass.
Aggregate exhaustion is deferred to another pass. A retained publication over
the per-session 8 MiB ceiling remains unavailable for this rename-only path;
cursor rotation cannot make it fit. Its prior name and pending transaction
remain intact. Real native content changes still use normal capture limits
and may publish the latest resolved label alongside the new conversation.

Lookup requires retained source and metadata to agree on the admitted session,
native identity and current machine. Missing or malformed metadata keeps labels
unavailable until ordinary capture or metadata refresh supplies ownership proof.
The private cache is machine-scoped and rechecks a content-free state token even
during lookup backoff; stale proof cannot trigger a name publication.

Context cache entries store only IDs, filtered evidence, producer/contract,
hashes, timestamps and scheduling counters. A name-only change publishes a
new filtered source and metadata through normal durable pending publication,
including the listing revision. Capture permission, frozen generations and
retention policy continue to govern whether retained evidence can be used.
The lookup cache holds at most 2,048 entries and 4 MiB of compact JSON. Eviction
does not remove a name already retained in published source evidence.

The shared collector, archive and private cache treat provider identity as an
opaque bounded value. Codex interprets its native UUID, producer and file
contract within its integration. A hash of provider interpretation, filter and
per-agent parser versions invalidates private lookup context. Earlier retained
file-contract evidence remains readable offline; it does not authorize a new
native lookup under the current contract.

The [deferred native comparison protocol](../../dev/maintainers/native-session-name-comparisons.md) tracks sidebar/list/show parity, restart, coexistence and history coverage with an explicitly pending evidence template. Lookup probes alone do not complete that acceptance.

## Explicit native opt-in

To enable native lookup, pause collection, back up the archive data directory's
`config.json`, and add `"codex_name_lookup": "native"` to that JSON object,
preserving all other fields and authorization history. Resume collection after
checking the JSON. Set `"files"` or remove the optional field to return to files.
Unknown values are rejected. Configuration schema remains unchanged; older
writers may drop this optional preference and safely return to files.

Native mode starts `codex app-server` through PATH only during eligible collector
work. Listing, showing, setup and status do not launch it. One lazy stdio process
per approved home is reused for a pass and closed afterwards. The only messages
are initialize, initialized and thread/read with includeTurns false; no thread
start/resume, turns, rename, tools or inventory requests are sent. The returned
home, native ID and pinned 0.159.2 protocol/retained producer are checked. Missing
optional names and unknown versions are unavailable; compatible null is explicit
absence. Known current environment or local configuration overrides and related
histories are refused. Managed/system configuration and historical SQLite
placement remain unverified: the child's `CODEX_SQLITE_HOME` selects the home
only as a fallback, and Codex's merged `sqlite_home` setting can override it.
Startup may therefore touch configured native storage beyond that fallback.

Opt-in accepts native startup side effects: disposable 0.159.2 probes observed
database/state and system-skill writes even with metadata-only requests. One
initialization exceeded eight seconds. Two synthetic servers coexisted in an
earlier probe; coexistence with the desktop app, real credential-store behavior,
live sidebar parity and realistic archive-backed native homes remain unverified.
This mode does not promise a read-only native host. It respects user native auth
settings through HOME/CODEX_HOME, but does not forward archive credentials, AWS
variables or shell API keys, and never executes login or approval requests.

Native work is serial, capped at 64 IDs and two seconds across the pass; startup
and initialization share that total budget, each read has at most 250 ms.
Responses are limited to 256 KiB per line and 1 MiB across hosts; stderr is
discarded with a 16 KiB cap. Cancellation closes pipes, kills and reaps with a
250 ms grace. Unavailable API reads may use the guarded file provider; file
absence cannot clear previously retained API evidence. Cache backoff and cursor
fairness remain the same as files mode. One measured warm lookup using installed
0.159.2, the production host and an invented owning thread in a disposable home returned the expected name in
598 ms; actual process reap and both reader exits passed. Cold instrumentation
failed before a lookup measurement. This single warm observation does not
establish a startup latency distribution or acceptance of the unresolved live
cases above; the runtime budgets remain enforced independently of it.
