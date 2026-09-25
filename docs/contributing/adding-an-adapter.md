# Adding an agent adapter

Supporting another coding agent means teaching agent-archive four things:
where its hooks go, what its hook payloads say, how to filter its transcript
safely, and how to read the filtered result back. Start from the closest
existing app (Codex for a JSONL CLI agent, Cursor for an editor) and follow
its trail through the packages below. Read the [architecture](architecture.md)
first.

## 1. Evidence first

Before writing code, collect real, synthetic-content transcripts and hook
payloads from the app, and record what you observed, for which version, in
[capture capabilities](../reference/capture-capabilities.md). Support stays
`unverified` until a session from an observed version is published end to
end. Don't infer a field from documentation alone.

## 2. The adapter (`internal/archive`)

- Add a type implementing `Adapter` (`Name`, `Version`, `FilterJSONL`) in
  `adapters.go`, and a case in `NewAdapter`. List the record types you
  accept explicitly; unknown record types are a capture gap, never passed
  through.
- Reuse `sanitizeObject`/`sanitizeValue`: the key allowlist, the blocked keys,
  tool-argument handling, value redaction, binary-block dropping, and the
  64 KB string cap. Add allowlist keys only for fields you have seen, with a
  comment saying why each is needed. Hidden reasoning and injected
  instructions must be dropped and reported as gaps.
- Add fixtures under `internal/archive/testdata/` named `<app>-<shape>.jsonl`
  (the round-trip test requires every fixture to have an app prefix), and
  regenerate `filter-golden.json` (`go test ./internal/archive
  -update-filter-golden`). Review every retained line.
- Teach the normalized view (`views.go`) and handoff (`handoff.go`) to find
  turns, prompts, and tool calls in the new records, with fixtures and
  golden output in `testdata/handoff/`.
- This is a filter change: bump `FilterVersion` and `adapterVersion` and
  document it (see [versions](../reference/versions.md)).

## 3. Hooks and setup (`internal/hooks`, `internal/cli`)

- `hooks`: where the app reads hook configuration, the events to install,
  and how to merge into its file without disturbing anything else.
- `cli`: detecting the app in setup, parsing its hook payloads in `_hook`
  (session ID, transcript path, working directory, start provenance), and
  its status lines. A hook must stay fast, silent, and offline.
- Session eligibility: decide how a never-seen session proves it started
  fresh (see [session eligibility](../reference/session-eligibility.md)); if
  the app can't say, don't guess.

## 4. The rest

- `internal/evidence`: the app's skill directories, if it has any.
- `internal/backfill`: discovering the app's existing transcripts.
- `internal/reader`: add the app to `Harnesses`, the key segments listings
  probe.
- `schemas/` and docs: the app name in enums, the README's supported apps,
  and [privacy](../security/privacy.md) for anything new that is uploaded.

## Tests to write

A fixture per record shape you retain or drop, a test that the unknown shapes
you saw are reported as gaps rather than retained, a hook payload test, a
setup round trip against a temporary `HOME` (see [testing](testing.md)), and
a handoff golden. Fuzz the new filter path briefly (`-fuzz` with
`-fuzzminimizetime 2s`).
