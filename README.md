# agent-archive

Store your agent sessions across Claude Code, Codex, and Cursor so that you
can analyze and recursively improve across all your agents.

`agent-archive` is a small macOS CLI that keeps a private, local-first archive
of your Codex, Claude Code, and Cursor sessions in a bucket you own. It
installs lifecycle hooks in each app, runs a background collector that uploads
session transcripts and metadata to a private Cloudflare R2 or Amazon S3
bucket, and prunes them on a retention schedule. No account or hosted service
is involved.

## Install

Download the binary for your Mac from the [latest release](https://github.com/wangjohn/agent-archive/releases/latest),
verify its checksum, and run `agent-archive setup`. Full steps, including
building from source and uninstalling, are in [docs/install.md](docs/install.md).

## Commands

- `agent-archive setup` guides first-time setup or a safe reconfiguration.
- `agent-archive status` shows storage, collector, hooks, and capture coverage.
- `agent-archive sync` runs one collection and upload pass now.
- `agent-archive pause` and `agent-archive resume` suspend and restore scheduled work.
- `agent-archive list` and `agent-archive show` inspect archived sessions from the bucket, metadata first.
- `agent-archive backfill` imports the sessions already on this Mac after showing a plan and asking; `backfill undo` removes an import from the bucket.
- `agent-archive handoff` prints a session as a prompt another coding agent can continue from, on this Mac or another.
- `agent-archive uninstall` removes hooks and the collector while keeping local evidence and credentials; `--delete-local-data` explicitly removes owned local data after confirmation. It never touches the bucket.

Run `agent-archive` alone for a short command guide, or `agent-archive COMMAND --help` for examples.

## Design

The [product and engineering specification](docs/agent-run-archive-spec.md)
describes the design. The [implementation ledger](docs/agent-archive-implementation.md)
records what has been built and verified.

## Develop

```sh
go test -race ./...
go vet ./...
VERSION=dev ./scripts/build-release.sh
```

CI runs the tests on Ubuntu and macOS. Pushing a `vX.Y.Z` tag builds, signs,
notarizes, and publishes a release. The release job fails before building unless
the Apple signing secrets and the `APPLE_SIGNING_ENABLED` repository variable
described in [docs/install.md](docs/install.md#signing-and-notarization) are set.

## History

This CLI was developed inside [wangjohn/agent-skills](https://github.com/wangjohn/agent-skills) before moving
to its own repository in September 2026.
