# Listing performance target

Ordinary noninteractive `list --limit N` and `list --json --limit N`
use session-addressed revision-qualified `listing/v3/` entries; existing
`listing/v2/` summaries remain readable. A complete healthy archive reads
at most N canonical metadata bodies on a cold run, or N selected cache files
on a warm run, and no transcript bodies. Both the 10,000 and 20,000 session
fixtures assert the default 50-body budget against the exhaustive reader.
Warm unchanged selections perform no remote metadata GETs. Normal metadata
refreshes share the same total body budget: a stale cached revision is never
opened before downloading its replacement. Cache paths encode the canonical
key and a SHA-256 of the opaque validator; header-only pruning removes old
versions. Flat legacy cache files are disposable cold misses and are removed
without opening their bodies. Cache key limits and corruption fallback remain
unchanged.

The budget covers bodies, not discovery: fresh canonical and both v2/v3 index LIST
headers are enumerated on every query, proportional to archive size. Text
uses activity time, excludes subagents before limiting, and obtains child
counts from covered summaries. JSON preserves capture-time order. Equal
capture times use harness and session identity as deterministic tie breakers. Implicit repository scope matches RepoKey
across clones and falls back to local ProjectIDs for legacy sidecars. Empty
scopes choose the existing all-project fallback before reading bodies.

Named explicit project scope, interactive browsing, search, origin, model, skill and
completeness queries remain exhaustive, as does `--limit 0`. A missing,
stale, unsupported or damaged index produces an explicit compatibility-scan
diagnostic and complete legacy results. A selected object deleted or changed
during a bounded query fails explicitly, suggesting retry or `--limit 0`;
it never refills indefinitely. Exact counts describe discovered matching
identities, not validation of every unselected sidecar.

## Revision protocol

Canonical metadata is authoritative. Keys are
`listing/v3/<harness>/<id>/<19-digit reverse Unix nanoseconds>/<summary>`.
Legacy v2 keys put the reverse timestamp before harness/id. Both forms
encode the same validated summaries; v3 needs no separate cleanup pointer.
The summary is canonical JSON encoded as unpadded base64url, containing the
fresh publication nonce (`n`), opaque provider ETag (`v`), SHA-256 of canonical bytes (`h`), activity timestamp
(`a`), optional parent ID (`p`), replay marker (`r`), ProjectID (`j`) and RepoKey (`k`). No source or skill
content is included. Unsupported/noncanonical summaries and keys exceeding
S3's 1,024-byte key bound are refused. Activity is EndedAt when present,
otherwise StartedAt for imports, otherwise CapturedAt.

Each live canonical header must have an entry matching exact key and opaque
ETag before any body is selected. No ready marker can establish coverage.
The selected response returns bytes and ETag together; its ETag, SHA-256,
canonical identity, schema and summary must match. Cache bytes are checked
against the same entry digest. ETags are never decoded as content hashes on
this path. Stores without response-bound validators use the exhaustive path.
S3 and R2 use their GetObject ETag; fake HTTP and opaque-validator tests cover
the protocol. Real provider acceptance remains a release acceptance task.

Publication journals a destination-bound per-session repair intent before
canonical commit. Successful capture survives an auxiliary failure; its
warning and repair intent remain visible. Collector passes retry at most 32
intents, rotating the cursor so a failing group cannot starve later intents.
Entries are empty immutable objects addressed directly under each session.
A v3 repair snapshots only that session's hint headers before publishing its
fresh identity, confirms the canonical validator, then retires at most 32
snapshot candidates. It leaves its intent until cleanup completes. One
normal publication reads one response-bound canonical metadata body and no
auxiliary bodies; it writes one hint rather than a pointer/hint pair. Delayed
writes and crash retries remain discoverable directly from session headers.
Concurrent snapshots cannot each include the other's later fresh identity,
so one current survivor remains even for equivalent writers or reused
validators. No logical counter or clock is needed.

`list --rebuild-index` snapshots legacy v2 headers and pointers before fresh
v3 publication and retires a combined maximum of 32 v2/v3 candidates per
session repair. Each v3 candidate needs one DELETE; legacy pairs need up to
two DELETEs (at most 64 calls for 32 candidates). Legacy partial pointers
can require at most one body GET per candidate; known hint keys require none. It reclaims legacy pointerless hints while canonical metadata
remains live. Repeating a partial rebuild resumes cleanup, including malformed
or canonical-absent hints (up to 32 per cleanup slice). It validates canonical
response bytes and writes/deletes only auxiliary objects; transcripts and
canonical metadata are unchanged. Failure reports completed metadata entries.
Legacy partial pointers are inspected only within that same candidate budget;
a corrupt pointer never authorizes deleting another session's claimed hint.

Retention and undo delete canonical discovery first, then the session's v3
entries and sources. They also enumerate legacy v2 hint headers to reclaim
pointerless artifacts, with a combined 32-candidate v2/v3 cleanup slice that
can be retried. Auxiliary failure never postpones source deletion. Archive-wide
legacy header enumeration is confined to explicit rebuild and deletion; normal
v3 publication never scans the full hint archive. Delayed old v2 writers may
recreate legacy artifacts, which remain discoverable by later explicit sweeps.
Equivalent current summaries are deduplicated across v2 and v3; conflicting
claims for one validator require compatibility scanning. A v2-only reader
cannot prove coverage for v3-only sessions and uses its exhaustive fallback.
Concurrent writers require no global lock. A concurrent rewrite invalidates
fresh coverage and produces compatibility scanning or a bounded-query error.

The benchmark in `internal/reader/list_index_bench_test.go` compares the cold
full scan, indexed cold selection and a real selected metadata cache. Timing
is supplementary; deterministic operation counts are the regression gate.

## Full scans

Every command that needs the whole archive runs the full scan: `list --limit 0`,
`stats`, bare `show`, the handoff picker, `handoff "<query>"`, and `list` on an
archive whose index isn't ready. On R2 a listing page of 1,000 keys takes
0.5–0.9 s, and one listing's pages are strictly sequential. So since the
[listing-at-scale](../proposals/listing-at-scale.md) phase 1 change, the scan is
split into contiguous key ranges listed concurrently: at most 16 in flight,
each about 200 of the sidecars the metadata cache knows (one page). The
ranges cover every key whatever the boundaries, so the cache only decides
how evenly the work is spread. With under 400 cached sidecars under the
listed prefix (a `--harness` listing counts only that harness's), or with
`--no-cache`, the scan is the single sequential listing.
