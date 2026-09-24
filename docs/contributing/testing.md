# Testing

```sh
go test -race ./...
go vet ./...
golangci-lint run                                        # v2.14.0; what CI blocks on
golangci-lint run --enable-only=revive --new-from-merge-base=origin/main
```

CI runs these on macOS and Ubuntu with Go 1.27.1 exactly (go.mod's
`toolchain` line), plus `govulncheck`. The Keychain code needs cgo and
Xcode's command line tools on macOS; elsewhere a stub is built.
`go test ./...` also runs `internal/doclinks`, which fails on a broken
relative link or `#anchor` in any Markdown file.

## Never test against your real Mac

Tests and hand-run experiments must not touch your real home directory, your
apps' real hook files, the real LaunchAgent, your Keychain, Cursor's real
database, or a real bucket. The live collector on your Mac is
`com.agent-archive.collector`.

In Go tests, everything goes through injection:

- `internal/cli` tests build an `Env` (see `testEnv` in `cli_test.go`) with a
  temporary data directory, temporary user and account homes, a fixed clock,
  no environment variables, and an in-memory bucket. Replace `runLaunchctl`
  with `stubLaunchctl` for anything that would load or stop a job.
- `internal/backfill` and `internal/cli` point Cursor database copies at a
  per-run temporary folder (`cursorstore.SnapshotTempDirForTesting`, set in
  their `TestMain`).
- Storage tests use `storage.NewMemoryStore()`.

## Running the binary by hand in a sandbox

```sh
scratch=$(mktemp -d)
mkdir -p "$scratch/home" "$scratch/stub"
printf '#!/bin/sh\necho "stub launchctl $*" >&2\nexit 0\n' > "$scratch/stub/launchctl"
chmod +x "$scratch/stub/launchctl"

export AGENT_ARCHIVE_HOME="$scratch/data"   # a data directory of its own
export HOME="$scratch/home"                 # app configs and LaunchAgents live here
export PATH="$scratch/stub:$PATH"           # agent-archive runs `launchctl` from PATH
```

- `AGENT_ARCHIVE_HOME` gives the sandbox its own data directory and its own
  launchd label; a sandboxed `HOME` keeps setup away from your real app
  configs. Neither stops a completed setup from loading its job into your
  real launchd, which is what the `launchctl` stub is for. Without the stub,
  cancel setup at "Start archiving?".
- Setup needs a terminal; `script -q /dev/null agent-archive setup` supplies
  one when you drive it from a script.
- For storage, run a local S3-compatible server (MinIO works) and point an
  AWS profile in the sandbox's `~/.aws/config` at it with
  `endpoint_url = http://127.0.0.1:9000`.
- To exercise the pipeline without setup, write `config.json` by hand (see
  [configuration](../reference/configuration.md)) and feed `agent-archive
  _hook --harness <app>` a JSON payload on stdin with a copied transcript path, then
  run `agent-archive sync`. Never register a real transcript path.
- `scripts/measure-hook.py BINARY` measures hook latency in its own temporary
  directory; it installs nothing.

The hidden commands `_hook` (what app hooks run) and `_collect` (what the
LaunchAgent runs) are not part of the user interface and may change.

## Fixtures and goldens

- Adapter fixtures are in `internal/archive/testdata/` as `<app>-<shape>.jsonl`
  with synthetic content only. `filter-golden.json` pins the SHA-256 of what
  each fixture filters to; Cursor database chats and handoff output have
  goldens of their own. Regenerate with `go test ./internal/archive
  -update-filter-golden -update-composer-golden -update` and review the diff
  line by line: a golden change is a privacy change (see
  [versions](../reference/versions.md)).
- Name tests by the behavior they pin (`TestUndoKeepsAProjectAnotherImportStillNeeds`),
  not by the review that found the bug.
- A bug fix comes with a test that fails without the fix. Check by reverting
  the fix.

## Fuzzing

Redaction and parsing have fuzz targets (`go test -list Fuzz ./...`). Their
seed corpora are checked in under `testdata/fuzz/`, and plain `go test` runs
the seeds. To fuzz, run each target for a couple of minutes, with fast
minimization so a failure is reported promptly:

```sh
go test ./internal/archive -run XXX -fuzz FuzzRedactSensitive -fuzztime 2m -fuzzminimizetime 2s
```

A failing input is written to `testdata/fuzz/<target>/`; keep it there as a
seed once it is fixed.
