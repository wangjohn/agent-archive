# Listing performance target

Ordinary unscoped, noninteractive `list --limit N` and `list --json --limit N`
use revision-qualified `listing/v2/` entries. A complete healthy archive reads
at most N canonical metadata bodies on a cold run, or N selected cache files
on a warm run, and no transcript bodies. Both the 10,000 and 20,000 session
fixtures assert the default 50-body budget against the exhaustive reader.
Warm unchanged selections perform no remote metadata GETs.

The budget covers bodies, not discovery: fresh canonical and index LIST
headers are enumerated on every query, proportional to archive size. Text
uses activity time, excludes subagents before limiting, and obtains child
counts from covered summaries. JSON preserves capture-time order. Equal
capture times use harness and session identity as deterministic tie breakers.

Project scope, interactive browsing, search, origin, model, skill and
completeness queries remain exhaustive, as does `--limit 0`. A missing,
stale, unsupported or damaged index produces an explicit compatibility-scan
diagnostic and complete legacy results. A selected object deleted or changed
during a bounded query fails explicitly, suggesting retry or `--limit 0`;
it never refills indefinitely. Exact counts describe discovered matching
identities, not validation of every unselected sidecar.

## Revision protocol

Canonical metadata is authoritative. Keys are
`listing/v2/<19-digit reverse Unix nanoseconds>/<harness>/<id>/<summary>`.
The summary is canonical JSON encoded as unpadded base64url, containing the
opaque provider ETag (`v`), SHA-256 of canonical bytes (`h`), activity timestamp
(`a`), optional parent ID (`p`) and replay marker (`r`). No source or skill
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
Entries are empty immutable objects; per-session pointers are written first
so interrupted writes stay discoverable by cleanup. A successful repair
retires at most 32 obsolete entries and leaves its intent until cleanup is
finished. Retention and undo delete canonical discovery first, then index
pointers and sources; an auxiliary failure does not postpone source deletion.

`list --rebuild-index` scans and validates canonical metadata using
response-bound validators, writes only auxiliary entries, and cleans invalid
v2 keys after successful validation. Writes are idempotent; rerunning resumes
a partial rebuild. A failure reports how many metadata entries completed.
Rebuild and publication never rewrite transcripts for index maintenance.
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
