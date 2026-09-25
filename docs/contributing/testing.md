# Testing

```sh
go test -race ./...
go vet ./...
golangci-lint run --disable=revive                       # v2.14.0; the blocking lint run
golangci-lint run --enable-only=revive --new-from-merge-base=origin/main   # doc comments, new code only
python3 scripts/test_release_signing.py
python3 scripts/test_install.py
python3 scripts/test_purge_recipe.py                     # runs the bucket purge recipes in the docs
```

CI (`test.yml`) runs the tests and scripts on macOS and Ubuntu with Go
1.27.1 exactly (go.mod's `toolchain` line), and golangci-lint on macOS: the
first run blocks, and revive's doc-comment rule runs only on code a pull
request adds or changes. The Keychain code needs cgo and Xcode's command
line tools on macOS; elsewhere a stub is built. `go test ./...` also runs
`internal/doclinks`, which fails on a broken relative link or `#anchor` in
any Markdown file.

## Levenshtein checks

The `verify` job (`levenshtein.yml`) runs the shared checks from
[wangjohn/levenshtein](https://github.com/wangjohn/levenshtein) at the
commit that workflow pins, in a Linux container: Go lint (staticcheck and
more) and vet, HTTP and SQL rules, `go.mod` hygiene, `govulncheck`,
`actionlint`, and `zizmor` for the workflows. `levenshtein.json` picks the
checks. Run the same thing from a sibling checkout of Levenshtein at the
pinned commit (it needs Docker):

```sh
../levenshtein/verify pre-merge --source .
```

The Go lint is the check most changes trip, and it runs natively in
seconds. Build its binary once from the pinned commit, then lint as the
container does, with Linux build tags and the checks list from
Levenshtein's `runner/toolchain.json`:

```sh
pin=$(sed -n 's/^ *ref: \([0-9a-f]\{40\}\)$/\1/p' .github/workflows/levenshtein.yml)
git -C ../levenshtein worktree add /tmp/levenshtein-pin "$pin"
(cd /tmp/levenshtein-pin/runner/lint && go build -o /tmp/levenshtein-lint ./cmd/levenshtein-lint)
GOOS=linux CGO_ENABLED=0 /tmp/levenshtein-lint \
  -checks 'all,-ST1000,-ST1003,-ST1016,-ST1020,-ST1021,-ST1022,-gocognit' ./...
```

It prints nothing when clean. Because it lints Linux build tags, the
darwin-only Keychain and launchd files are covered by the macOS
golangci-lint job instead. Two things it enforces that surprise people:
a `//lint:ignore nilerr` directive never works (restructure the code
instead), and terminal output goes through `internal/terminal` rather than
`_, _ = fmt.Fprintf`.

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
cat > "$scratch/stub/launchctl" <<'EOF'
#!/bin/sh
# Stand-in for launchctl: no job is ever loaded, and nothing reaches launchd.
echo "stub launchctl $*" >&2
if [ "$1" = print ]; then
  echo "Could not find service \"$2\" in domain for port" >&2
  exit 113
fi
exit 0
EOF
chmod +x "$scratch/stub/launchctl"

export AGENT_ARCHIVE_HOME="$scratch/data"   # a data directory of its own
export HOME="$scratch/home"                 # app configs and LaunchAgents live here
export PATH="$scratch/stub:$PATH"           # agent-archive runs `launchctl` from PATH
# Variables that would point setup back at your real configuration:
unset CLAUDE_CONFIG_DIR CODEX_HOME AWS_CONFIG_FILE AWS_SHARED_CREDENTIALS_FILE
```

- `AGENT_ARCHIVE_HOME` gives the sandbox its own data directory and its own
  launchd label; a sandboxed `HOME` keeps setup away from your real app
  configs. Neither stops a completed setup from loading its job into your
  real launchd, which is what the `launchctl` stub is for. The stub answers
  `print` as launchd does for a job that isn't loaded ("Could not find
  service", exit 113); a stub that just exits 0 there leaves the job's state
  unknown, and setup refuses to continue. Without the stub, cancel setup at
  "Start archiving?".
- `CLAUDE_CONFIG_DIR` and `CODEX_HOME` would send setup's hooks to your real
  app configuration even with a sandboxed `HOME`; the `AWS_*` file variables
  would use your real AWS profiles. Unset them.
- Choose S3 (a local MinIO, below) in a sandbox, not R2: R2 secrets are
  saved in your real login Keychain, which a sandboxed `HOME` doesn't change.
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
