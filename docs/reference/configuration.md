# Configuration

`agent-archive setup` writes the configuration; every other command reads
it. It lives in `config.json` in the data directory (see
[local state](local-state.md)) and holds no secrets. Edit it through setup,
not by hand: setup checks storage, rewrites hooks, and keeps the fields
consistent with each other.

## Fields

| Field | Meaning |
| --- | --- |
| `schema_version` | Shape of this file. Currently `1`. |
| `machine_id` | This Mac's random identity, written into every session it captures. Kept across reconfiguration, and copied with the data directory by Migration Assistant or a backup restore; see [multiple Macs](../guides/multiple-macs.md#migration-assistant-and-time-machine). |
| `storage.Provider` | `s3` or `r2`. |
| `storage.Bucket`, `storage.Prefix` | The bucket and the folder inside it (`agent-archive/` unless changed in setup). Every object key is under the prefix; see [bucket layout](bucket-layout.md). |
| `storage.Region` | S3 region (from the AWS profile when it has one). |
| `storage.AWSProfile` | S3 only: the named AWS profile credentials are loaded from. No other credential source is used. |
| `storage.R2AccountID`, `storage.R2Endpoint` | R2 only: the account or S3-compatible endpoint. |
| `storage.R2CredentialRef` | R2 only: the Keychain reference (service `agent-archive`) of the access key. The secret itself is in the Keychain. |
| `archive.enabled` | Whether hooks and the collector capture anything. |
| `archive.projects[]` | Included and excluded projects: `root` (resolved path), `project_id` (derived from the root), `included`, and `activated_at`. Only sessions admitted at or after a project's activation are captured for it. |
| `harnesses` | Apps setup installed hooks for: `claude`, `codex`, `cursor`. |
| `imported_harnesses` | Apps whose sessions `backfill` imported without their hooks installed. |
| `declined_harnesses` | Apps you declined when setup offered them; setup doesn't offer them again. |
| `hook_files` | Per app, the hook file setup installed into, resolved from `CLAUDE_CONFIG_DIR` and `CODEX_HOME` at setup time. |
| `installed_executable` | The `agent-archive` path written into the hooks and the LaunchAgent. |
| `retention_days` | Whole-session retention in days: 90 by default, 1 to 36,500. There is no "keep forever". |
| `require_skill_use` | When `true`, only sessions that used a skill are captured. Default `false`: all sessions. |
| `paused` | Set by `pause`, cleared by `resume`. |
| `destination_since`, `previous_destinations` | When the current storage destination was configured, and the ones it replaced. Sessions stay with the destination they were published to. |
| `storage_verified_at`, `bucket_privacy` | The last storage access check and bucket privacy inspection. Evidence, not settings. |
| `retired_credential_refs` | Keychain references of R2 credentials a reconfiguration replaced, kept so `uninstall --delete-local-data` can remove them too. |

## Environment variables

| Variable | Effect |
| --- | --- |
| `AGENT_ARCHIVE_HOME` | Data directory instead of `~/.local/share/agent-archive`. It must not be inside a Git checkout. A non-default directory gets its own launchd label (`com.agent-archive.collector.<hash>`), and setup writes it into the hook commands, since apps run hooks without your shell's environment. Each installation changes only the hooks that carry its own directory; setup refuses to install beside another installation's hooks, so give a second or test installation its own `HOME` too (or `CLAUDE_CONFIG_DIR` and `CODEX_HOME`). |
| `CLAUDE_CONFIG_DIR` | Claude Code's configuration directory, where setup installs hooks (`settings.json`). Read when setup runs; recorded in `hook_files`. |
| `CODEX_HOME` | Codex's home, where setup installs hooks (`hooks.json`). Read when setup runs; recorded in `hook_files`. |
| `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE` | Where the AWS SDK finds profiles, as for the AWS CLI. The background collector runs under launchd, which passes it none of your shell's environment, so for S3 setup writes the ones set in its shell (as absolute paths) into the collector's LaunchAgent, along with that shell's `PATH` (its absolute entries, then launchd's `/usr/bin:/bin:/usr/sbin:/sbin`), so a `credential_process` such as `aws-vault`, 1Password's `op`, or `granted` in `/opt/homebrew/bin` runs there too. Change either and run setup again; `status` warns when the collector's files are gone, its `PATH` no longer finds the profile's `credential_process`, or your shell's files differ from the collector's. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN` are never copied: the profile supplies credentials. |
| `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID` | Set by the app for commands it runs; `handoff --latest` skips that session. |
| `NO_COLOR` | Disables colored output. |
| `AGENT_ARCHIVE_VERSION`, `AGENT_ARCHIVE_INSTALL_DIR` | `install.sh` only: the release and directory to install. |

Cursor's hook file is always `~/.cursor/hooks.json`.
