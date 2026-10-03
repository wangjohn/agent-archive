# Versions

Several independent version numbers describe what produced an archived
session. Each is recorded with the session, so a reader can tell which rules
applied, and a change to any of them makes the collector re-derive what it
affects.

| Version | Where | Recorded in | Current | Changes when |
| --- | --- | --- | --- | --- |
| Release | `cli.Version` (set at build time) | `--version` | Build-dependent (`dev-<commit>` from source) | A tag is cut. |
| Filter | `archive.FilterVersion` | source header `capture.filter_version`, metadata `filter_version` | 15 | What the privacy filter keeps, drops, or redacts changes: any change to filtered output. |
| Adapter | `adapterVersion` in `internal/archive/adapters.go` | `capture.adapter_version` | 0.15.0 | An adapter's output changes (bumped with the filter in practice). |
| Parser | `archive.DefaultParserVersion` | metadata `parser.version` | 0.20.0 | How metadata is derived from a source changes: counts, turns, models, skills, gaps, titles, session names, branch, linked pull requests, tools used, end time, tokens (per model too), tool errors, MCP calls, git activity, the repository key (derived from the project's git origin, not the source). |
| Source schema | `archive.SourceSchemaVersion` | source header `schema_version` | 2 | The source bundle's line format changes. Readers refuse other versions. |
| Metadata schema | `archive.MetadataSchemaVersion` | metadata `schema_version` | 1 | The metadata sidecar changes incompatibly. Optional fields don't bump it. |
| Machine record | `machines.SchemaVersion` | `machines/<machine_id>.json` and `machines --json` | 1 | Informational registry format changes incompatibly; independent of session schemas and filtering. |
| Configuration | `config.SchemaVersion` | `config.json` `schema_version` | 1 | `config.json` changes incompatibly. |
| List JSON | `cli.listSchemaVersion` | `list --json` `schema_version` | 4 | The script-facing list document changes incompatibly. Version 3 removed `unavailable`; version 4 makes exact-count knowledge explicit. |
| Status JSON | `statusView.Version` in `internal/cli/status.go` | `status --json` `schema_version` | 4 | Status output changes incompatibly; schema 4 separates discovery from actual hook evidence. |

The per-version filter changes are in the
[filter changelog](../specs/privacy-filter-changelog.md).

## What a bump does

The collector compares each session's last scan (its scan signature) with the
running build's parser, filter, and adapter versions. When the filter or
adapter differs, it re-reads the transcript, filters it again, and
republishes the session. When only the parser changed, it republishes
metadata derived from the retained source, and reads the transcript only
if it changed since the last scan (an unchanged one would filter to
exactly what was retained); a changed one is published as usual. Nothing
is re-uploaded unless it changed.

## Rules for contributors

- **Any change to filtered output** — a key kept or dropped, a redaction
  pattern, a new block type — bumps `FilterVersion` and adds a section to the
  [filter changelog](../specs/privacy-filter-changelog.md), updates the
  [filter specification](../specs/privacy-filter.md), and, if what users
  should know changes, [privacy](../../docs/security/privacy.md). Regenerate the goldens
  ([how](../contributing/testing.md#fixtures-and-goldens)) and review the
  diff: every changed line is a privacy decision.
  Bump `adapterVersion` in the same change.
- **Any change to derived metadata** (counts, turn detection, model or skill
  attribution) bumps `DefaultParserVersion`.
- **Schema changes** need the JSON schemas in `schemas/` updated in the same
  change, and a reader that still reads what earlier versions wrote, or a
  clear refusal.
- Update the version line in this file.

Discovery authorization uses an incompatible schema-version object
`{version: 2, writer: discovery-floor-v2}`. It retains an immutable
`native_start_floor` for each permission generation, including generations
created while paused. The earlier `discovery-v2` writer cannot preserve this
boundary and must refuse the new fence. Hook-only configuration retains numeric
version 1; the current decoder reads both protected writer identities and
refuses unknown fences. Every save and nested setup snapshot writes the current
fence; identity writing upgrades an earlier protected fence first.

An older nonempty history migrates conservatively from its earliest retained
interval. An older empty history has no recoverable consent boundary: it may
be preserved and fenced, but cannot resume or authorize starts. Setup
reconciliation renews it as a fresh generation at the latest of reconciliation,
project activation, and destination activation. It does not reconstruct old
consent from activation alone. Future scope-policy schemas must preserve these
floors and the stronger writer fence through all saves and rollback snapshots.

Codex capture scope and local admission proof use the stronger incompatible
`{version: 3, writer: codex-scope-floor-v3}` configuration fence. It remains installed
when scope is reduced or discovery disabled, and on rollback over protected
history. New readers retain numeric/v2 included-project consent without
opening blanket windows. This changes session consent and physical project
attribution, not adapter filtering, native parsing or derived-field algorithms;
filter, adapter, parser and published source/metadata schema versions remain
unchanged. The private policy/proof contract is documented in configuration.

Strict JSON Schema validators using the v0.1.1 schemas reject new discovery
provenance: source schema 2 disallows additional properties, and metadata
schema 1 constrains origin to import. Use the updated schemas shipped with
the new release. Ordinary typed readers accept older records and optional
new provenance; the source line format remains 2 and metadata remains 1.
Optional metadata fields do not bump those schema versions; parser version
20 records the changed derivation.
