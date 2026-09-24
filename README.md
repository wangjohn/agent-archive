# agent-archive

[![Test](https://github.com/wangjohn/agent-archive/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/wangjohn/agent-archive/actions/workflows/test.yml)
[![Go 1.27](https://img.shields.io/badge/go-1.27.1-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Keep your Claude Code, Codex, and Cursor sessions in a bucket you own, so you
can search them, compare them, and hand one agent's work to another.

`agent-archive` is a small macOS command-line tool. It installs lifecycle
hooks in each app, runs a background collector that filters each session's
transcript and uploads it with its metadata to a private Cloudflare R2 or
Amazon S3 bucket, and deletes old sessions on a retention schedule. There is
no account, hosted service, or telemetry.

> **Status: pre-release.** There is no signed release yet — build from
> source. Interfaces and the bucket layout may still change before `v0.1.0`.

## What leaves your Mac

- **Your sessions, filtered:** prompts, the agent's replies, tool calls with
  their arguments and results (commands, file paths, edit bodies), working
  directories (which usually contain your username), Git branch names,
  models, and token counts. Only projects you include are captured.
- **Kept out:** hidden reasoning, system and injected instructions, images
  and other binary content, and every field the filter doesn't know.
- **Redacted, best effort:** recognizable credentials (`DB_PASSWORD=…`,
  `"api_key": …`, AWS keys, GitHub and Slack tokens, private keys, JWTs,
  passwords in URLs). A secret with no recognizable name or shape is archived
  as it appears.
- **Only to your bucket.** There is no client-side encryption: anyone who can
  read the bucket can read your sessions. Use narrow credentials
  ([bucket permissions](docs/security/bucket-permissions.md)).
- **On your Mac,** it edits the `hooks` entry of each app's settings file,
  adds one LaunchAgent, and keeps its state in `~/.local/share/agent-archive`.
  R2 secrets are in the Keychain.

Details, and where the protections stop: [privacy](docs/security/privacy.md).

## How it works

```mermaid
flowchart LR
  A["Claude Code · Codex · Cursor"] -- "lifecycle hooks" --> H["agent-archive _hook<br/>records the session"]
  H --> S[("local state<br/>~/.local/share/agent-archive")]
  L["LaunchAgent<br/>every 60 s"] --> C["collector<br/>filter → bundle → upload → verify"]
  S --> C
  C --> B[("your S3 / R2 bucket")]
  B --> R["list · show · handoff"]
  T["transcripts already<br/>on this Mac"] --> BF["backfill"] --> S
```

The [architecture](docs/contributing/architecture.md) page has the details.

## Quickstart

```sh
git clone https://github.com/wangjohn/agent-archive.git
cd agent-archive
VERSION=dev ./scripts/build-release.sh              # needs Go 1.27.1 and Xcode command line tools
mv ./dist/agent-archive-darwin-$(uname -m | sed 's/x86_64/amd64/') /opt/homebrew/bin/agent-archive

agent-archive setup          # choose apps and projects, connect a private bucket
agent-archive status         # check capture, then start a new agent session
agent-archive backfill       # optional: import sessions already on this Mac
```

You need an existing private R2 or S3 bucket. See
[install](docs/getting-started/install.md) and
[setup](docs/getting-started/setup.md).

A `status` run, from a sandboxed test installation:

```text
Agent Archive — Needs attention

Storage:       s3 / test-bucket / agent-archive/
Access checked: never
Bucket privacy not verified.
  Checked: never; inspection_unavailable.
  Review: https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html
Authentication: verified (checked 2026-01-02T00:00:00Z; manual_sync)
Background:    loaded
Projects:      1 included
Pending:       0 session(s)
Last scan:     2026-01-02T00:00:00Z
Last publish:  2026-01-02T00:00:00Z
```

and `list`:

```text
SESSION                           HARNESS  CAPTURED              ORIGIN  PARSER   MODELS    SKILLS USED
d0a8124edb786e5686a068deca82e04f  codex    2026-01-02T00:00:00Z  hook    partial  gpt-test  -
1 session(s).
```

## Commands

- `setup` guides first-time setup or a safe reconfiguration.
- `status` shows storage, collector, hooks, and capture coverage.
- `sync` runs one collection and upload pass now.
- `pause` and `resume` suspend and restore scheduled work.
- `list` and `show` inspect archived sessions from the bucket, metadata first.
- `handoff` prints a session as a prompt another coding agent can continue from, on this Mac or another.
- `backfill` imports the sessions already on this Mac after showing a plan; `backfill history` and `backfill undo` review and remove imports.
- `feedback` attaches your own assessment to a session.
- `uninstall` removes hooks and the collector; `--delete-local-data` also removes local data. It never touches the bucket.

Run `agent-archive` or `agent-archive help` for a short guide, `agent-archive
help COMMAND` or `agent-archive COMMAND --help` for options and examples, and
`agent-archive --version` for the version. Commands exit 0 on success, 1 when
something failed, and 2 for a usage error.

## Supported platforms

- macOS on Apple Silicon and Intel. Linux builds and tests run in CI, but
  capture is macOS-only (launchd, Keychain).
- Storage: Cloudflare R2, Amazon S3.
- Apps: Claude Code, Codex, and Cursor. Versions the adapters were built
  and checked against: Claude Code 2.1.x, Codex CLI 0.155, Cursor 3.21.13.
  Other versions are
  reported as `unverified` rather than assumed to work; see
  [capture capabilities](docs/reference/capture-capabilities.md).

## Documentation

The [documentation index](docs/README.md) lists every guide and reference
page. Start with [install](docs/getting-started/install.md),
[privacy](docs/security/privacy.md), and
[troubleshooting](docs/guides/troubleshooting.md).

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) — never test against your real Mac;
the [testing](docs/contributing/testing.md) page has a sandbox recipe.
Report vulnerabilities privately as [SECURITY.md](SECURITY.md) describes.
Changes are recorded in [CHANGELOG.md](CHANGELOG.md). This project follows a
[code of conduct](CODE_OF_CONDUCT.md).

This CLI was developed inside
[wangjohn/agent-skills](https://github.com/wangjohn/agent-skills) before
moving to its own repository in September 2026. It is released under the
[MIT License](LICENSE).
