# agent-archive documentation

Start with the [README](../README.md) for what agent-archive is. The docs
below are grouped by who they are for.

## Using agent-archive

| Doc | For |
| --- | --- |
| [Install](getting-started/install.md) | Building from source (pre-release) or installing a release. |
| [Setup](getting-started/setup.md) | Choosing apps and projects, connecting a bucket, and what setup changes. |
| [Uninstall](getting-started/uninstall.md) | Removing hooks, the collector, local data, and the binary; deleting the archive in the bucket; downgrading. |
| [Backfill](guides/backfill.md) | Importing sessions already on your Mac, and undoing an import. |
| [Handoff](guides/handoff.md) | Continuing a session in another agent, on this Mac or another. |
| [List, show, and feedback](guides/list-and-show.md) | Inspecting the archive; skill evidence; subagent sessions. |
| [Multiple Macs](guides/multiple-macs.md) | Several Macs sharing one bucket; Migration Assistant and Time Machine. |
| [Troubleshooting](guides/troubleshooting.md) | Reading `status`, recovering an interrupted setup, changing storage, upgrading. |
| [FAQ](guides/faq.md) | Short answers: cost, deleting everything, which versions and platforms work, transcript format changes. |

## Security and privacy

| Doc | For |
| --- | --- |
| [Privacy](security/privacy.md) | Threat model, what is and isn't uploaded, cleaning up after a filter upgrade, what changes on your Mac, the filter rules. |
| [Bucket permissions](security/bucket-permissions.md) | Least-privilege S3 policy and R2 token. |
| [Filter changelog](security/filter-changelog.md) | What each filter version changed. |
| [SECURITY.md](../SECURITY.md) | Reporting a vulnerability. |

## Reference

| Doc | For |
| --- | --- |
| [CLI reference](reference/cli.md) | Every command, its help, its flags, and the exit codes (generated from the CLI). |
| [Configuration](reference/configuration.md) | `config.json` fields and environment variables. |
| [Bucket layout](reference/bucket-layout.md) | Object keys and how objects change. |
| [Local state](reference/local-state.md) | Every file in the data directory. |
| [Versions](reference/versions.md) | Filter, adapter, parser, and schema versions, and when each is bumped. |
| [Capture capabilities](reference/capture-capabilities.md) | What each app's hooks provide, and the versions observed. |
| [Session eligibility](reference/session-eligibility.md) | Which sessions are captured, and why. |
| [JSON output](reference/json-output.md) | `list --json`, `show`, and `status --json`: the contract for scripts. |
| [Glossary](reference/glossary.md) | The terms agent-archive uses: harness, capture gap, sidecar, admission, parser status, read-back, and more. |
| [JSON schemas](reference/schemas.md) | `metadata.schema.json` and `source-bundle.schema.json`, their $ids, and the tests that validate them. |

## Contributing

| Doc | For |
| --- | --- |
| [CONTRIBUTING.md](../CONTRIBUTING.md) | How to contribute, and the rules for privacy-sensitive changes. |
| [Architecture](contributing/architecture.md) | How the pieces fit, and the package map. |
| [Testing](contributing/testing.md) | Tests, lint, fuzzing, and a sandbox that never touches your real Mac. |
| [Adding an adapter](contributing/adding-an-adapter.md) | Supporting another coding agent. |
| [Session admission](contributing/session-admission.md) | How a registration's start and admission times drive each boundary check, and the guard tests. |
| [Releasing](maintainers/releasing.md) | Tagging, signing, and notarization (maintainers). |

## Design and history

Design documents explain why things are the way they are; each says how far
it is implemented.

- [Archive design](design/archive-spec.md), the original product and engineering specification.
- [Handoff design](design/handoff.md) and [backfill design](design/backfill.md).
- Proposed, not implemented: [cloud capture](design/proposed/cloud-capture.md).

The history folder keeps working records for context. They are not
maintained: the [implementation ledger](history/implementation-ledger.md),
the [CLI plan](history/cli-plan.md), the
[audit remediation plan](history/audit-remediation-plan.md) and its
[acceptance record](history/remediation-acceptance.md), the
[gap plan](history/gap-implementation-plan.md), and the
[backfill implementation plan](history/backfill-implementation-plan.md).
