# agent-archive development

Specifications, contributor guides, and maintainer runbooks. For using
agent-archive, see the [user documentation](../docs/README.md); to contribute,
start with [CONTRIBUTING.md](../CONTRIBUTING.md).

## Contributing

| Doc | For |
| --- | --- |
| [Architecture](contributing/architecture.md) | How the pieces fit, and the package map. |
| [Testing](contributing/testing.md) | Tests, lint, fuzzing, and a sandbox that never touches your real machine. |
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
| [Agent skills](specs/agent-skill.md) | Implemented; follow-ups listed in the spec. |
| [Cloud capture](proposals/cloud-capture.md) | Proposed; not implemented. |
| [Portable handoff, guided setup, and Linux](proposals/portable-handoff-and-onboarding.md) | Implemented (repo-key handoff, guided S3 and R2 creation, Linux); guided R2 is experimental. Open items in the document. |
| [Platform abstraction](proposals/platform-abstraction.md) | Implemented (scheduler port, OS value, systemd backend, Linux support); open items in the document. |
| [Git activity in metadata](proposals/git-activity.md) | Implemented in parser 0.15.0. |
| [List and browse UX](proposals/list-browse-ux.md) | Design record; implementation tracked in PRs #99–#101. |
| [Archive listing at scale](proposals/listing-at-scale.md) | In progress: phase 1 (parallel range listing) of 4. |

## Maintainers

| Doc | For |
| --- | --- |
| [Releasing](maintainers/releasing.md) | Tagging, signing, and notarization. |
| [Published-release acceptance](maintainers/open-source-acceptance.md) | Disposable Mac and bucket smoke test with recorded pass, fail, or pending evidence. |
| [Versions](maintainers/versions.md) | Filter, adapter, parser, and schema versions, and when each is bumped. |
