# Testing

```sh
go test -race ./...
go vet ./...
golangci-lint run --disable=revive                       # v2.14.0; the blocking lint run
golangci-lint run --enable-only=revive --new-from-merge-base=origin/main   # doc comments, new code only
go run golang.org/x/tools/cmd/deadcode@v0.50.0 ./...     # only the exceptions listed in test.yml
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

Performance tests check what a pass costs on every run (published-state
decodes and local writes, counted, not timed). Their wall-clock targets run
only at full size in a plain build, which CI does in a step of its own:

```sh
AGENT_ARCHIVE_PERF=1 go test -count=1 -run 'StayFast|FiveMegabyte' ./internal/collector ./internal/archive
```

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
  no environment variables, and an in-memory bucket. Its launchd, Keychain,
  and executable fields fail the test unless the test sets them. Replace
  `runLaunchctl` with `stubLaunchctl` for anything that would load or stop a
  job.
- Isolation in `internal/cli` fails closed. Its `TestMain` points `$HOME` at a
  temporary folder, unsets `AGENT_ARCHIVE_HOME`, `CLAUDE_CONFIG_DIR`,
  `CODEX_HOME` and the AWS configuration variables, and replaces the real
  `launchctl` and Keychain with stand-ins that stop the test (see
  `isolation_test.go`). A test that leaves an `Env` field unset can therefore
  never reach your real apps, launchd, or Keychain.
- `internal/credentials` fails closed too: its `TestMain` replaces every
  Keychain call `KeychainStore` makes with one that stops the test, so a
  test can reach the real login Keychain only through the opt-in
  `TestKeychainRoundTrip` (`AGENT_ARCHIVE_KEYCHAIN_ROUND_TRIP=1`).
- `internal/backfill` and `internal/cli` point Cursor database copies at a
  per-run temporary folder (`cursorstore.SnapshotTempDirForTesting`, set in
  their `TestMain`).
- Storage tests use `storagetest.NewMemoryStore()` (`internal/storage/storagetest`, test code only: depguard keeps it out of production code, as it does `state/statetest`).

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
  each fixture filters to; Cursor database chats
  (`internal/archive/testdata/cursor-composer/`), handoff output
  (`testdata/handoff/`), backfill plans (`internal/cli/testdata/backfill/`,
  `internal/backfill/testdata/`) have goldens of their own.
- One flag rewrites every golden file:

  ```sh
  go test ./... -update                  # or one package: go test ./internal/archive -update
  git diff                               # review every changed line
  ```

  Review the diff line by line: a change to the archive's goldens is a
  privacy change (see [versions](../reference/versions.md)). The flag is
  defined once, in `internal/testutil/golden` (`golden.Check`,
  `golden.Update`), which every package with tests imports, so no package
  rejects it (`TestEveryTestedPackageKnowsUpdate`). A new golden test uses
  `golden.Check`; a package with tests but no goldens imports the package
  blank.
- A bug fix comes with a test that fails without the fix. Check by reverting
  the fix.

## Where tests live

- A test sits next to the production file it covers: tests of `status.go` go
  in `status_test.go`, or in `status_<aspect>_test.go` when one aspect has
  enough tests to be worth its own file (`status_gaps_test.go`,
  `setup_transaction_recovery_test.go`). Package-wide test helpers
  (`testEnv`, fixtures used by many files) live in `cli_test.go` or the
  package's `testonly_test.go`; `TestMain` lives in `main_test.go`.
- Name files and tests by the behavior they pin
  (`TestUndoKeepsAProjectAnotherImportStillNeeds`), never by the review,
  round, or pull request that found the bug: no `review_fixes_test.go`,
  `second_review_test.go`, or `cli_correctness_test.go`.
- Record where a regression came from in the test's doc comment instead:

  ```go
  // A resumed chat keeps the project it started in.
  //
  // Regression: 2026-09 review H-21.
  func TestResumeKeepsItsProject(t *testing.T) {
  ```

- Moving tests between files is a pure move: keep each test's body
  unchanged, and check that `go test -list '.*' ./...` prints the same names
  before and after.

## Parallel tests

Tests call `t.Parallel()` unless they cannot share the process: a test that
assigns a package variable (`stubLaunchctl`, `collectSoftDeadline`,
`hookDiagnosticsWait`), calls `t.Setenv` or `os.Chdir`, reads a process-wide
counter (`state.PublishedStateLoads`), removes this process's Cursor
snapshots, or orders goroutines with real sleeps stays sequential, with a
comment saying why when it is not obvious. Go runs every sequential test
before it releases the parallel ones, so a package variable a sequential
test changes and restores is never seen by a parallel test. Test seams
that vary per test belong in `Env` (`observeFlags`, `backfillCheckpoint`,
`exitOnSignal`), not in package variables. Subtests that each build their
own fixture call `t.Parallel()` too.

Check a change for order dependence and races with
`go test -race -count=3 -shuffle=on ./internal/cli`.
`internal/cli`'s `TestMain` puts `$TMPDIR` (so every `t.TempDir`) in a
folder of the run's own under `/tmp`: `local.CanonicalPath` lists each parent
of a path, and macOS's per-user temporary folder can hold thousands of
entries.

## Fuzzing

Redaction, parsing, hook-file editing and the hook itself have fuzz targets
(`go test -list Fuzz ./...`). In `internal/archive`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzRedactSensitive` | any string | redacting twice changes nothing; valid UTF-8 stays valid |
| `FuzzRedactCredentialTemplates` | a secret in each credential shape | the secret never survives |
| `FuzzSanitizeValueIdempotent` | any string | the whole string sanitizer (JSON inside strings, instruction blocks, redaction, the cap) is idempotent |
| `FuzzFilterJSONL` | any JSONL, with each adapter (Claude Code, Codex, Cursor) | no panic; only `FilterError`s; retained records are JSON objects that refilter unchanged; the handoff renders with no control character |
| `FuzzFilterJSONLDropsSecrets` | a secret in typed input, credential-named arguments, and JSON strings, per adapter | the secret never survives |
| `FuzzCursorText` | any Cursor text transcript | refiltering is a no-op; no hidden section is retained; the handoff finds no more prompts than the filter kept |
| `FuzzCursorComposer` | a Cursor database chat and one message row | no panic; retained records are JSON objects; the handoff renders cleanly |
| `FuzzDecodeSource` | any byte stream, gzip or not | no panic; the streaming and whole-bundle readers agree |
| `FuzzLineMatchesCoverWholeString` | any text | running each credential pattern line by line, only on lines with its needles, finds everything matching the whole string finds |
| `FuzzGatedRedactionMatchesWhole` | any text | the redaction as it runs (line by line, gated by needles and separators) equals the same redaction run ungated over the whole string |

In `internal/hooks`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzMergeRemove` | any hook file Merge accepts, per harness | merging twice changes nothing; Remove takes out exactly what Merge added |
| `FuzzCommandDataHome` | any data directory | the hook command reads back the directory it was built with |

In `internal/cli`:

| Target | Input | Properties |
| --- | --- | --- |
| `FuzzHookPayload` | an app name and up to eight hook payloads, run in turn against a fresh data directory set up for all three apps | every run exits 0 without a panic, and nothing outside the data directory (the project folder, `HOME`) changes |

Their seeds come from the fixtures in `testdata/` and from
`testdata/fuzz/<target>/`, and plain `go test` runs them. CI runs every target
in every package for 30 seconds on every pull request (the `fuzz` job in
`.github/workflows/test.yml`). For a change to redaction or parsing, run the
affected targets for a couple of minutes each, with fast minimization so a
failure is reported promptly:

```sh
go test ./internal/archive -run '^$' -fuzz '^FuzzFilterJSONL$' -fuzztime 2m -fuzzminimizetime 2s
```

A failing input is written to `testdata/fuzz/<target>/`; keep it there as a
seed once it is fixed.
