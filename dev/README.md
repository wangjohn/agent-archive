# agent-archive development

Specifications, contributor guides, and maintainer runbooks. For using
agent-archive, see the [user documentation](../docs/README.md); to contribute,
start with [CONTRIBUTING.md](../CONTRIBUTING.md).

## Contributing

| Doc | For |
| --- | --- |
| [Architecture](contributing/architecture.md) | How the pieces fit, and the package map. |
| [Testing](contributing/testing.md) | Tests, lint, fuzzing, and a sandbox that never touches your real Mac. |
| [Adding an adapter](contributing/adding-an-adapter.md) | Supporting another coding agent. |
| [Session admission](contributing/session-admission.md) | How a registration's start and admission times drive each boundary check, and the guard tests. |

## Specifications

Each says at the top how far it is implemented; where a spec and the code
differ, the code and the user documentation describe current behavior.

| Spec | Status |
| --- | --- |
| [Archive](specs/archive.md) | The original product and engineering specification; mostly implemented. |
| [Privacy filter](specs/privacy-filter.md) | Implemented: every rule the filter applies. |
| [Privacy filter changelog](specs/privacy-filter-changelog.md) | What each filter version changed, and why. |
| [Backfill](specs/backfill.md) | Implemented. |
| [Handoff](specs/handoff.md) | Implemented. |
| [Cloud capture](proposals/cloud-capture.md) | Proposed; not implemented. |

## Maintainers

| Doc | For |
| --- | --- |
| [Releasing](maintainers/releasing.md) | Tagging, signing, and notarization. |
| [Versions](maintainers/versions.md) | Filter, adapter, parser, and schema versions, and when each is bumped. |
