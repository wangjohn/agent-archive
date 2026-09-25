# Bucket layout

Everything agent-archive writes is under the prefix you chose in setup (shown
here as `<prefix>/`; with no prefix, keys start at `sessions/`).

```text
<prefix>/
  sessions/
    <harness>/                       claude, codex, or cursor
      <archive-session-id>/
        metadata.json                the session's metadata sidecar (the live pointer)
        source.<sha256>.jsonl.gz     a filtered source snapshot; <sha256> is of these bytes
        source.<sha256>.jsonl.gz     an earlier snapshot, until retention removes it
  .setup-test/<random>.json          a connection test object, deleted within seconds
  .setup-test/clock-<random>.json    a clock check before retention deletes anything, deleted at once
```

- **Archive session ID.** 32 lowercase hex characters, assigned on the Mac
  that captured the session. It is not the app's own session ID, which is
  kept inside the metadata (`native_session_id`).
- **`metadata.json`** is small JSON (schema:
  [`schemas/metadata.schema.json`](../../schemas/metadata.schema.json)): the
  session's identity, machine, project ID, app and version, capture time,
  counts, models, skills, capture gaps, parser and filter versions, and the
  key, SHA-256, and size of the current source. `list` reads only these.
- **`source.<sha256>.jsonl.gz`** is gzip of newline-delimited JSON (source
  schema 2; [`schemas/source-bundle.schema.json`](../../schemas/source-bundle.schema.json)):
  a header line, one line per retained native record, then text transcripts
  and supplemental evidence (hook observations, skill snapshots, links to
  subagent sessions). Only what the [privacy filter](../security/privacy.md)
  kept is in it. `show --normalized` and `handoff` read it and check its
  SHA-256 and identity against the metadata.

## How objects change

- A session is published by uploading the new source first, then replacing
  `metadata.json` to point at it, then reading both back to verify them. A
  reader always finds a complete source behind the metadata it reads.
- The previous source is kept (the immediate predecessor always, older ones
  for a 24-hour grace period), then deleted by the Mac that owns the session.
- When a session expires (retention, 90 days by default) or `backfill undo`
  removes it, its metadata is deleted before its sources, so an interruption
  leaves at worst unreferenced source objects for the next pass, never a
  pointer to missing data.
- Both deletions go by age, measured by the Mac's clock, so before either
  the Mac checks its clock against the storage service's (the modification
  time of a `.setup-test/clock-*` object it writes and deletes). While the
  Mac's clock is more than an hour ahead, or the service's clock can't be
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
