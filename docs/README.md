# agent-archive documentation

Start with the [README](../README.md) for what agent-archive is. These docs
are for people using it; specs and contributor guides are in
[`dev/`](../dev/README.md).

## Using agent-archive

| Doc | For |
| --- | --- |
| [Install](getting-started/install.md) | Installing a release, or building from source. |
| [Create a bucket](getting-started/bucket.md) | A private R2 or S3 bucket and an access key, step by step. |
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
| [Privacy](security/privacy.md) | Threat model, what is and isn't uploaded, cleaning up after a filter upgrade, what changes on your Mac. |
| [Bucket permissions](security/bucket-permissions.md) | Least-privilege S3 policy and R2 token. |
| [SECURITY.md](../SECURITY.md) | Reporting a vulnerability. |

## Reference

| Doc | For |
| --- | --- |
| [CLI reference](reference/cli.md) | Every command, its help, its flags, and the exit codes (generated from the CLI). |
| [Configuration](reference/configuration.md) | `config.json` fields and environment variables. |
| [Bucket layout](reference/bucket-layout.md) | Object keys and how objects change. |
| [Local state](reference/local-state.md) | Every file in the data directory. |
| [Capture capabilities](reference/capture-capabilities.md) | What each app's hooks provide, and the versions observed. |
| [Session eligibility](reference/session-eligibility.md) | Which sessions are captured, and why. |
| [JSON output](reference/json-output.md) | `list --json`, `show`, and `status --json`: the contract for scripts. |
| [Glossary](reference/glossary.md) | The terms agent-archive uses: harness, capture gap, sidecar, admission, parser status, read-back, and more. |
| [JSON schemas](reference/schemas.md) | `metadata.schema.json` and `source-bundle.schema.json`, their $ids, and the tests that validate them. |
