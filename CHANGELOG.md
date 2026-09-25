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
  bucket, reads it back to verify it, and deletes sessions past the
  retention period (90 days by default).
- **`setup`**: three steps (apps and projects, storage, review), resumable if
  interrupted, and safe to rerun to change any of them. R2 secrets are kept
  in the macOS Keychain; S3 uses an AWS profile.
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
  Hugging Face, and npm tokens; private keys, also base64-encoded as in a
  kubeconfig; Docker registry logins; JWTs; passwords in URLs, `curl -u`,
  and `.netrc`). See the
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
  privacy as `not_verified`; check the bucket in the Cloudflare dashboard.
- Sessions are not encrypted by agent-archive; anyone who can read the
  bucket can read them.

[Unreleased]: https://github.com/wangjohn/agent-archive/commits/main
