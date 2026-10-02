# Bucket layout

Everything agent-archive writes is under the prefix you chose in setup (shown
here as `<prefix>/`; with no prefix, keys start at `sessions/`).

```text
<prefix>/
  machines/
    <machine_id>.json               informational machine name, platform, credential claim, daily heartbeat
  sessions/
    <harness>/                       claude, codex, or cursor
      <archive-session-id>/
        metadata.json                the session's metadata sidecar (the live pointer)
        source.<sha256>.jsonl.gz     a filtered source snapshot; <sha256> is of these bytes
        source.<sha256>.jsonl.gz     an earlier snapshot, until retention removes it
  .setup-test/<random>.json          a connection test object, deleted within seconds
  .setup-test/clock-<random>.json    a clock check before retention deletes anything, deleted at once
  listing/
    v1-ready                       written after a complete index rebuild
    v1-needs-rebuild               older bucket detected by an uploading machine
    v1/<reverse-time>/<app>/<id>/<hash>.json  immutable listing hint
    by-session/<app>/<id>/<hash>    cleanup pointer for a listing hint
```

- **Archive session ID.** 32 lowercase hex characters, assigned on the machine
  that captured the session. It is not the app's own session ID, which is
  kept inside the metadata (`native_session_id`).
- **`metadata.json`** is small JSON (schema:
  [`schemas/metadata.schema.json`](../../schemas/metadata.schema.json)): the
  session's identity, machine, project ID, repository key (a hash of the
  git origin, when there is one), app and version, capture time,
  counts, models, skills, capture gaps, parser and filter versions, and the
  key, SHA-256, and size of the current source. `list` reads only these.
- **`listing/`** holds time-ordered hints. A limited `list` pages through
  these keys and verifies each candidate against its current `metadata.json`
  before showing it. Index entries never contain conversation content and
  can be stale after republish or deletion. `list --rebuild-index` scans an
  older bucket's sidecars and writes the `v1-ready` marker last. Until then,
  limited listing uses the full sidecar scan. Retention and undo remove a
  session's hints using the `by-session` pointers. Every uploading machine must
  run an index-aware collector before rebuilding, and continue to do so afterward.
  v0.1.1 collectors and external writers that publish metadata without a hint
  are not supported alongside an enabled index: their new sessions may be absent
  from bounded JSON listings. The ready marker does not detect those writers.
  For a development bucket written by those tools, use `list --limit 0` until
  they have stopped, then rebuild. Current collectors publish each hint before
  its metadata, so a new session is discoverable as soon as it is published.
  If a hint is damaged, listing falls back to a full sidecar scan; rerun
  `list --rebuild-index` to repair the index.
- **`source.<sha256>.jsonl.gz`** is gzip of newline-delimited JSON (source
  schema 2; [`schemas/source-bundle.schema.json`](../../schemas/source-bundle.schema.json)):
  a header line, one line per retained native record, then text transcripts
  and supplemental evidence (hook observations, skill snapshots, links to
  subagent sessions). Only what the [privacy filter](../security/privacy.md)
  kept is in it. `show --transcript` and `handoff` read it and check its
  SHA-256 and identity against the metadata.

The configured `skill_evidence` mode affects newly built source bundles.
`none` carries no filesystem skill inventory or snapshots, `metadata` carries
names and filtered hashes, and `body` also carries filtered snapshots. A
policy change does not erase already uploaded source objects. A replaced
source may remain as a predecessor or in bucket version history; removing
old bytes requires reviewing those copies as well as the live pointer.
`agent-archive purge plan` lists unreferenced source keys and reports current
older-filter sessions separately. Its private plan and report are local; they
are not new bucket objects. [Privacy cleanup](../security/privacy.md#after-a-filter-upgrade)
explains how to apply a plan with every uploading machine paused.

## How objects change

- A session is published by uploading the new source first and verifying
  it in storage, then replacing `metadata.json` to point at it. A reader
  always finds a complete source behind the metadata it reads. The upload
  carries the source's SHA-256, which the service checks before it stores
  anything, and the verification asks the service (a `HEAD` request) for
  the SHA-256 and size of the object now at the key, so it downloads
  nothing; a service that reports no checksum has the source read back and
  hashed instead. A source already stored with the same bytes is not
  uploaded again.
- The previous source is kept (the ordinary immediate predecessor always,
  older ones for a 24-hour grace period). When a filter-version change
  republishes a session, its old-filter predecessor is marked for cleanup
  after the new publication passes read-back verification and 24 hours have
  elapsed. A failed delete is retried by that machine's retention sweep.
- When a session expires (retention, 90 days by default) or `backfill undo`
  removes it, its metadata is deleted before its sources, so an interruption
  leaves at worst unreferenced source objects for the next pass, never a
  pointer to missing data.
- Both deletions go by age, measured by the machine's clock, so before either
  the machine checks its clock against the storage service's (the modification
  time of a `.setup-test/clock-*` object it writes and deletes). While the
  machine's clock is more than an hour ahead, or the service's clock can't be
  read, nothing is deleted by age and `status` says why; a clock that jumped
  more than a day since the previous pass waits one pass. The check runs only
  when something is due for deletion, and its reading is reused for up to an
  hour while it holds deletion (ten minutes while it allows it), so a clock
  that stays wrong costs one check object an hour. In a bucket with
  versioning on, each check leaves a noncurrent version and a delete marker
  under `.setup-test/`; a lifecycle rule that expires noncurrent versions
  there clears them.
- Nothing outside `<prefix>/` is read or written, and objects are never made
  public. See [bucket permissions](../security/bucket-permissions.md).

Object keys are built only from the app name and the archive session ID,
both checked to be safe key components; no path or native ID from a
transcript ever becomes part of a key.

## Machine records

`machines/` stores informational records under canonical machine IDs. The
[machine record schema](../../schemas/machine.schema.json) describes the format.
They contain chosen names, OS/architecture, application version, nonsecret
credential provenance and pairing details when locally committed, plus a daily
heartbeat. They contain no project paths or session content. Any bucket writer
can modify them, so they are not an authorization source. Session listing,
retention and privacy purge ignore this folder. Records remain after uninstall;
include `machines/` when deleting the entire archive.
