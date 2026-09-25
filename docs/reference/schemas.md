# JSON schemas

The two documents agent-archive writes to the bucket have JSON Schemas
(draft 2020-12) in [`schemas/`](../../schemas):

| Schema | `$id` | Describes |
| --- | --- | --- |
| [`metadata.schema.json`](../../schemas/metadata.schema.json) | `https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/metadata.schema.json` | A session's `metadata.json` sidecar, and each item of `list --json`'s `sessions`. `additionalProperties` is false. |
| [`source-bundle.schema.json`](../../schemas/source-bundle.schema.json) | `https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/source-bundle.schema.json` | One line of a `source.<sha256>.jsonl.gz` bundle (source schema 2): the header, a native record, a text transcript, or a piece of supplemental evidence. Ordering rules a line schema can't express are in its `description`. |

The `$id`s resolve to the files on `main`. Pin a commit in the URL if you
validate against a fixed version.

## What the schemas promise

- Enumerations (parser status, lifecycle state, turn outcome, skill
  coverage, evidence kinds, and capture gap codes) list exactly the values
  this build writes. Readers should still accept values they don't know:
  a later version may add one, and older sidecars may carry retired ones.
- A change that removes or redefines a field bumps the document's
  `schema_version` (see [versions](../../dev/maintainers/versions.md)); adding an optional field
  does not.

## How they are kept honest

`internal/archive/schema_test.go` validates, with
`santhosh-tekuri/jsonschema/v6`:

- every line of every fixture's source bundle, and its metadata sidecar as
  both a hook-captured and an imported session
  (`TestFixtureOutputMatchesPublishedSchemas`);
- that each schema enum matches the Go constants value for value, reading
  the constants from the source (`TestSchemaEnumsMatchGoConstants`);
- that both schemas list exactly `archive.CaptureGapCodes`, and every gap
  code the package writes is in it.

`show` output is not an instance of `metadata.schema.json`: it adds
`linked_session_availability` (see [JSON output](json-output.md#show)).
