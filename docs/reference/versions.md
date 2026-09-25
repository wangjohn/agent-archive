# Versions

Several independent version numbers describe what produced an archived
session. Each is recorded with the session, so a reader can tell which rules
applied, and a change to any of them makes the collector re-derive what it
affects.

| Version | Where | Recorded in | Current | Changes when |
| --- | --- | --- | --- | --- |
| Release | `cli.Version` (set at build time) | `--version` | pre-release | A tag is cut. |
| Filter | `archive.FilterVersion` | source header `capture.filter_version`, metadata `filter_version` | 11 | What the privacy filter keeps, drops, or redacts changes: any change to filtered output. |
| Adapter | `adapterVersion` in `internal/archive/adapters.go` | `capture.adapter_version` | 0.11.0 | An adapter's output changes (bumped with the filter in practice). |
| Parser | `archive.DefaultParserVersion` | metadata `parser.version` | 0.11.0 | How metadata is derived from a source changes: counts, turns, models, skills, gaps. |
| Source schema | `archive.SourceSchemaVersion` | source header `schema_version` | 2 | The source bundle's line format changes. Readers refuse other versions. |
| Metadata schema | `archive.MetadataSchemaVersion` | metadata `schema_version` | 1 | The metadata sidecar changes incompatibly. Optional fields don't bump it. |
| Configuration | `config.SchemaVersion` | `config.json` `schema_version` | 1 | `config.json` changes incompatibly. |

The per-version filter changes are in the
[filter changelog](../security/filter-changelog.md).

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
  [filter changelog](../security/filter-changelog.md) and, if the baseline
  changes, to [privacy](../security/privacy.md). Regenerate the goldens
  ([how](../contributing/testing.md#fixtures-and-goldens)) and review the
  diff: every changed line is a privacy decision.
  Bump `adapterVersion` in the same change.
- **Any change to derived metadata** (counts, turn detection, model or skill
  attribution) bumps `DefaultParserVersion`.
- **Schema changes** need the JSON schemas in `schemas/` updated in the same
  change, and a reader that still reads what earlier versions wrote, or a
  clear refusal.
- Update the version line in this file.
