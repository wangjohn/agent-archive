# Agent Archive next release implementation plan

Status: historical planning input, captured for the 2026-10-04 implementation
run. The plan below was written against `d85766f`; it does not describe the
current implementation at `2075c84327214c13e45295c2e1c32df0833cf878`.
Consult [release remediation](../proposals/release-remediation.md),
[listing at scale](../proposals/listing-at-scale.md), and the
[current CLI reference](../../docs/reference/cli.md) for later work and
current behavior. This record preserves the reviewed input and its completion
criteria; it is not release sign-off.

Current reconciliation: the pager already keeps immutable rendered bytes and
replays them on startup failures, including shell exits 126/127. Ordinary pager
exits, signals, and Ctrl-C retain their later-defined behavior without replay.
The quickstart already requires Codex hook approval, a fresh session, and an
app-specific archived, verified Capture row. JSON listing is now schema version
4, and repository scope means full-archive analytics must also pass
`--all-projects`. Pinned installation examples remain on published v0.1.1
until a tested candidate is promoted; candidate publication and disposable
provider/app acceptance are separate release gates.

The remaining text is the original proposed plan, retained without changing
its historical assumptions.

This plan addresses the review of changes from v0.1.1 through d85766f. The
recommended sequence fixes destructive cleanup and pager failures first,
adds bounded metadata downloads for ordinary listings, provides a safe
recovery path for blocked transcripts, and establishes evidence before a
release becomes the public latest version. This is a proposed implementation
plan; none of the product changes below is implemented by this document.

## Product decisions

- Preserve newest capture first, using `CapturedAt`, with a deterministic
  harness and session ID tie breaker. Do not substitute storage modification
  time: parser-only refreshes preserve capture time while rewriting metadata.
- Make the common indexed `list --limit N` fetch at most N metadata bodies
  in a healthy archive. Include both remote downloads and local sidecar reads
  in cost tests. Transcript bodies are never read by list.
- Keep results correct across old installations, multiple Macs, retention,
  and publication failures. An incomplete index must never silently hide
  sessions or be mistaken for an empty archive.
- Keep `--limit 0` as the explicit exhaustive path. Advanced filters may use
  that reader internally until there is a proven efficient query path.
- Make release readiness depend on a tested signed candidate and passing
  final-commit CI. A timeout or pending acceptance row is not a pass.

## Implementation sequence

| Change | Scope | Dependency | Completion evidence |
| --- | --- | --- | --- |
| 1 | Preserve pager output on failure | None | Actual subprocess and consumed-input regression tests |
| 2 | Correct cleanup pause gates and full-prefix deletion | None | bash and zsh failure matrix against a disposable fake bucket |
| 3 | Repair onboarding and document compatibility | None | Commands and generated references checked against the candidate |
| 4 | Prototype and specify bounded listing | None | Correctness and request-count comparison with the exhaustive reader |
| 5 | Publish and rebuild listing entries | 4 | Crash recovery, migration, mixed-writer and deletion tests |
| 6 | Enable bounded default reads and finalize CLI output | 5 | Default listing parity and explicit operation budgets |
| 7 | Add safe recovery for blocked transcripts | State and metadata design review | Existing history preserved and future activity captured after recovery |
| 8 | Stage releases and finish validation | All release-bound changes | Passing CI and signed-candidate acceptance record |

Keep these changes reviewable as separate pull requests. The listing writer
and reader should land in that order, with the existing exhaustive reader
remaining available during rollout.

## Preserve pager output

In `internal/cli/pager.go`, render the listing once and keep its bytes
unchanged. Pass an independent reader to the pager and a fresh reader to
fallback. Preserve ordinary successful quit behavior and propagation of
stdout write failures. Warnings must not claim fallback succeeded if writing
it failed.

Test a pager that consumes all input and fails, one that consumes part of it
and fails, and a real missing shell command. Verify complete fallback output,
warning text, and exit status. Retain tests for redirected output, JSON,
`--no-pager`, environment precedence, and successful pager exit.

## Make cleanup safe and usable for damaged archives

Restructure the documented preparation block so a failed local pause cannot
produce a valid deletion plan. Clear any previous plan before attempting
pause. State that every uploading installation must report a successful
pause; this recipe cannot establish that remote writers have stopped.
Keep pause failure actionable by preserving the lock-holder advice.

Separate full-prefix deletion from selective deletion:

- Full-prefix deletion validates the bucket, prefix, complete key listing,
  plan expiry, and exact reviewed targets. It does not require readable
  metadata or existing source references, because every scoped key is being
  deliberately removed.
- Unreferenced-source, old-filter, and machine deletion retain strict
  metadata and source-identity validation. An unknown reference must never
  be treated as permission to delete a source.
- Delete metadata pointers before sources in full-prefix mode as well, so a
  partial failure leaves fewer live pointers to missing data.
- Preserve single-use plans, changed-listing checks, exact-key deletion,
  progress logs, partial-failure reporting, and the explicit whole-bucket
  meaning of an empty prefix. Preserve the separate versioned-bucket caveat.

Expand `scripts/test_purge_recipe.py` with failed pause after a previously
valid plan; failed pause with a writer simulated in flight; malformed,
unreadable, and missing metadata; absent current sources; unrelated objects;
empty prefixes; and deletion failures at each object class. The same damaged
archive must be refused by selective mode and removable by full-prefix mode.

## Restore a complete onboarding path

Restore Codex hook approval in README quickstart, require a fresh session,
and verify the app's archived and verified capture row. Include a short
pre-capture privacy disclosure covering best-effort redaction, visible
user-level skills, and lack of client-side encryption, with the detailed
privacy link.

Document the default cap and the analytics command
`agent-archive list --json --limit 0`. Update pinned installation examples
with the final release process, without claiming a candidate is already
published. Run command, relative-link, and generated-reference checks.

## Bound listing downloads without changing ordering

### Why the current layout needs discovery information

Canonical metadata lives at `sessions/<harness>/<session-id>/metadata.json`.
Capture time is in the body. The storage interface's `List` returns headers
and follows every page; the reader then loads and sorts metadata.

Taking the first N keys, requesting N results from a storage page, or sorting
by `LastModified` cannot establish the newest N captures. A local catalog can
accelerate subsequent queries, but cannot avoid the first complete read on a
new Mac. Do not advertise those approaches as bounded cold reads.

### Recommended prototype

Prototype a versioned, auxiliary namespace of compact listing entries.
Prefer entries whose object keys encode capture time, canonical metadata
identity, and an opaque metadata version validator, with empty bodies. This
allows discovery from LIST headers without downloading one index file per
session. Define a canonical encoding and validate key lengths and characters.
Do not add transcript or skill content to this index.

Keep canonical metadata authoritative. Match listing entries to a fresh
listing of canonical metadata headers using exact key and version identity.
Ignore orphaned or obsolete entries. Detect canonical objects without a
matching supported entry, including writes from an old collector. This
coverage check is the reason the initial implementation still enumerates
headers rather than stopping after the first index page.

Do not assume ETags are MD5 hashes. The storage layer should expose the
provider's opaque version validator from the metadata PUT response, or obtain
it through a supported stat operation. The prototype must establish usable
validator behavior on both S3 and R2. If a store cannot provide reliable
validators, use the exhaustive reader rather than making a freshness claim.

For a complete index and default filters, order the current entries, select
N identities, and read only those N canonical sidecars. Reuse the existing
metadata cache for selected entries. Validate metadata identity, version,
schema, and capture time before returning it. Treat a concurrent rewrite as
a bounded retry or an explicit incomplete query; do not return stale metadata
under a newer listing entry.

The prototype is a decision gate, not a commitment to an unproven storage
format. Proceed only if entries can prove coverage and freshness at acceptable
publication cost. If that cannot be achieved simply, ship the correctness
fixes separately and retain an honest exhaustive listing until the design is
ready. Do not replace correct ordering with a heuristic to meet a benchmark.

### Cost and query contract

| Query or archive state | Planned behavior | Cost guarantee |
| --- | --- | --- |
| Complete index, default filters, limit N | Select entries before opening metadata bodies | At most N sidecar body reads in the healthy case; zero transcript reads |
| Warm selected cache | Read only selected cached sidecars | No remote metadata GETs for unchanged selected entries |
| Harness filter | Narrow canonical discovery and select matching entries | Same body bound when index coverage is complete |
| Since filter | Use indexed capture times | Same body bound when index coverage is complete |
| Model, skill, completeness, or origin filters | Keep the exhaustive implementation initially | Explicitly documented scan; optimize only after indexed predicate equivalence is proven |
| Limit zero | Exhaustive compatibility path | Complete results; no bounded body-read promise |
| Legacy or incomplete index | Correct compatibility scan with a clear diagnostic, or explicit refresh for a hard read budget | No silent claim that N bodies were read |

The common-path promise bounds files read, not all remote operations. Header
enumeration remains proportional to archive size. Bounding LIST pages as well
would require a further completeness protocol and a genuinely queryable
ordered index. That is a separate optimization with its own migration and
multi-writer design.

Use a bounded read/retry budget for selected corrupt, deleted, or changing
objects. Do not refill indefinitely after errors. Return an explicit partial
or incomplete state with a useful next step. Finalize whether a caller asking
for a hard read ceiling must opt into a separate budget; `--limit` alone
cannot promise N body reads for advanced filters and compatibility scans.

Exact `total_matched` and current validation of every sidecar must not force
the default path to download every body. Design JSON output around whether
the result is exhaustive and whether an exact count is known. A likely
contract is `returned`, `limit`, `more_available`, and nullable or omitted
`total_matched`, with explicit query completeness. Define corresponding text
footers. Provide an explicit exhaustive count operation if needed.

The list schema version 2 is currently unreleased relative to v0.1.1. Settle
this contract before publishing it. If version 2 has shipped by implementation
time, bump the schema rather than changing its fields incompatibly.

### Publication and migration

Publication remains source verification followed by canonical metadata.
Write a matching listing entry after canonical metadata commits. Persist
enough intent to recover every crash boundary. Capture success must not be
lost because an auxiliary listing write failed; retain repair work and expose
pending listing maintenance when it affects user experience.

Use immutable entries per publication instead of a shared mutable global
manifest. Multiple Macs must not need a global write lock. Persist and retire
superseded entry keys through retryable maintenance. Readers ignore stale
entries even when maintenance is delayed. Account for parser-only metadata
refreshes, backfill, undo, retention, destination changes, and full-prefix
purge. Index maintenance must never extend source retention.

Provide an explicit rebuild operation for existing buckets. It scans and
validates canonical metadata, writes only auxiliary entries, and reports
progress, failures, and resumable completion. It never rewrites transcripts
or canonical metadata. Confirm exact command naming in the CLI design.
Avoid turning every ordinary list command into an implicit remote write.

During migration, preserve complete legacy results with a clearly identified
compatibility scan. Old writers can invalidate coverage at any time; use
fresh canonical headers to detect that condition. An archive must not acquire
a permanent completeness flag that older writers cannot invalidate.

### Listing acceptance tests

Use the exhaustive reader as the behavioral oracle. Test archives with
hundreds and thousands of sessions, equal capture timestamps, late backfills,
parser-only refreshes, arbitrary key order, multiple pages, and multiple Macs.
Cover publication crashes before and after canonical metadata and entry
writes, failed entry repairs, mixed old/new writers, missing and corrupt
entries, metadata deleted or rewritten during a query, opaque validators,
and unknown schemas.

Instrument LIST pages, metadata GETs, cache file opens, source GETs, bytes,
and index publication operations. In a complete healthy archive with 10,000
sessions, default limit 50 must read no more than 50 sidecar bodies and no
transcript bodies. Doubling archive size must not increase body reads.
Measure cold and warm runs separately and report header enumeration honestly.
Verify advanced-filter results and all JSON/footer states against their stated
exhaustiveness contract.

## Recover blocked transcripts without sacrificing history

Do not weaken `nativeEvidenceExtends` to compare only record counts or file
size. That would accept compacted-and-grown transcripts over richer retained
history. Keep automatic recovery when current filtering proves extension.

Add a clear status reason and user-directed recovery for the ambiguous case.
The recommended first implementation starts a new linked archive generation
from the current native transcript, with a new archive identity, and keeps
the previous generation readable. Explain the gap and the history being
preserved before confirmation. Future hook activity must route to the new
generation exactly once; feedback and existing handoffs remain attached to
their original identities.

Before coding, specify native-to-archive identity mapping, linked subagents,
repeated recovery, retention independence, and import interactions. Journal
the transition under collector and hook locks so a crash cannot lose both
generations or create duplicate registrations. Keep normal retention policy;
linking generations must not silently keep expired content forever.

Test recovery of the known filter-upgrade mismatch, continued new records,
genuine compaction, unchanged transcripts, interruption at each transition,
and repeated invocation. Audit parser, adapter, filter, and metadata schema
version requirements against `versions.md`; do not bump filter versions
unless filtering behavior changes.

## Establish release evidence and reliable test execution

Publish the signed candidate as a prerelease, exercise the pinned installer
and published assets in disposable accounts, then promote the same tested
assets after acceptance. Promotion must not rebuild or replace them. Record
the final commit, architecture digests, signatures, notarization, provenance,
and provider/app acceptance results. Keep protected signing and promotion
permissions and make retries idempotent. Public documentation and latest
installation guidance must match the promoted version.

The local review passed vet, Python suites, both architecture builds, focused
listing/documentation race tests, and collector/storage race tests after
retry. The large-session performance case passed alone after an initial
concurrent run failed. Full CLI race tests timed out twice at ten minutes.
Local lint could not run with the installed v1 linter against the required
v2 configuration. These observations do not establish a passing final CI run.

Profile CLI tests with test timing output and inspect repeated backfill
fixtures and crash matrices. Remove redundant fixture work while preserving
crash-boundary coverage and production durability assertions. Use disposable
state only; do not remove production fsync or disable race tests to make the
suite fast. If heavy suites need separate jobs or an explicit timeout, justify
the change with measured runtime and preserve every required check.

Run timing assertions without concurrent builds or other heavy tests.
Deterministic operation counts are the main listing regression gates;
wall-clock tests supplement them on documented hardware. Run the required
v2 linter and the existing fuzz matrix in CI on the final commit. Finish the
S3/R2, app-by-app, Intel/Apple Silicon, clean-user, upgrade, recovery, and
cleanup acceptance rows with actual evidence. Pending checks remain pending.

## Release completion criteria

- Cleanup cannot create a valid plan after pause failure and can erase a
  reviewed full prefix even when its metadata is damaged.
- Pager fallback always preserves the complete listing.
- Quickstart produces a verified fresh capture, including Codex approval.
- Indexed default listing preserves capture-time order and meets its body
  read budget. Legacy and advanced-filter behavior is explicit and correct.
- Migration, repair, retention, undo, and multiple writers cannot silently
  omit sessions or leave unbounded auxiliary objects.
- Ambiguous transcript recovery preserves old history and captures future
  activity through a crash-safe, tested transition.
- JSON compatibility and analytics migration guidance match the release.
- Final-commit CI passes and signed-candidate acceptance is recorded before
  promotion to the public latest release.
