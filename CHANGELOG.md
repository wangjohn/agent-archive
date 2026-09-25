# Changelog

All notable changes are recorded here, by what they mean for you. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

The first release, `v0.1.0`, will be cut from this section.

### Added

- **Capture** of Claude Code, Codex, and Cursor sessions on macOS through
  each app's lifecycle hooks, for the projects you include. A LaunchAgent
  filters each session, uploads it with its metadata to your own S3 or R2
  bucket, verifies what arrived by its checksum, and deletes sessions past
  the retention period (90 days by default). A long, growing session costs
  a pass a few seconds, and nothing is downloaded to verify it.
- **`setup`**: three steps (apps and projects, storage, review), resumable if
  interrupted, and safe to rerun to change any of them. It offers the
  projects your apps already have sessions in, accepts the R2 bucket URL the
  Cloudflare dashboard shows, and ends with what each app needs next.
  `setup --yes` takes its answers as flags, to script a second Mac. R2
  secrets are kept in the macOS Keychain; S3 uses an AWS profile, and the
  collector runs with the AWS settings files, CA bundle, endpoint overrides,
  proxy settings and `PATH` setup verified it with (never AWS keys or tokens), so a
  `credential_process` like `aws-vault` or `op` works in the background
  too; `status` says when they stop working. Setup refuses a temporary
  binary, such as the one `go run` deletes on exit, and after a plain
  `uninstall` it sets up again with your saved answers.
- `agent-archive` with no arguments says when setup hasn't run yet.
- **`status`**: storage, collector, hook, and per-app capture health, with a
  `Next:` line whenever something needs you. `--json` for scripts.
- **`list`, `show`, `handoff`**: browse the archive from any Mac sharing the
  bucket, and print a session as a prompt another agent can continue from.
- **`backfill`**: import sessions already on this Mac after showing a plan;
  `backfill history` and `backfill undo` review and remove imports.
- **`sync`, `pause`, `resume`, `feedback`, `uninstall`**.
- **`install.sh`**, a one-line installer that checks the release checksum
  and Developer ID signature.
- A [CLI reference](docs/reference/cli.md) generated from the CLI, a
  [glossary](docs/reference/glossary.md), and
  [JSON schemas](docs/reference/schemas.md) validated against real output.

### Security

- **Privacy filter 12.** Hidden reasoning, system instructions, binary
  content, and unknown fields are never uploaded. Credentials are redacted
  by name (`DB_PASSWORD=`, `"api_key":`, `--token`, form fields labelled
  password or PIN) and by shape (AWS, GitHub, Slack, Stripe, GitLab, Google,
  Hugging Face, npm, Groq, and xAI tokens; private keys, also
  base64-encoded as in a kubeconfig; Docker registry logins; JWTs; access
  key IDs; wallet seed phrases; passwords in URLs, `curl -u`, `.netrc`, and
  `.pgpass`). Skill hashes are of the redacted text, so they can't confirm a
  guessed secret. See the
  [filter changelog](docs/security/filter-changelog.md).
- Hook files are edited in place: only agent-archive's own `hooks` entries
  change, and a file that can't be edited safely is left alone.
- Handoff output is quoted so session text can't escape it or drive your
  terminal.
- Release binaries are built from `main` after the tests pass, then signed,
  notarized, and attested in a separate protected job.

### Known limitations

- macOS only. Redaction is best effort: a secret with no recognizable name
  or shape is archived as it appears.
- R2's S3 keys can't read bucket privacy settings, so `status` reports R2
  privacy as `not_verified`, and setup reminds you to check that public
  access is disabled in the Cloudflare dashboard.
- Sessions are not encrypted by agent-archive; anyone who can read the
  bucket can read them.
- The background collector gets no other shell settings: an S3 profile
  that needs a `credential_process` helper's own variables
  (`AWS_VAULT_BACKEND`, `OP_ACCOUNT`) or a proxy with a password in its URL
  works for `sync` but not in the background. See
  [configuration](docs/reference/configuration.md#environment-variables).

[Unreleased]: https://github.com/wangjohn/agent-archive/commits/main
