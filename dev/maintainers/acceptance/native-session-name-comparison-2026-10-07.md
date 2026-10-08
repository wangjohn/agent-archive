# Native session name comparison execution, 2026-10-07

Result: **PARTIAL — synthetic pipeline checks passed; native UI acceptance
BLOCKED**. This does not close [#343](https://github.com/wangjohn/agent-archive/issues/343).
The [comparison protocol](../native-session-name-comparisons.md) remains the
acceptance authority. No real session was renamed, resumed, or sent a model
turn, and no login, auth copy, native configuration change, setup, sync or
backfill was performed against the real account.

## Candidate and modes

Base: `bfbc1f4ac57cfcf5b123def820ff161cc8dd5d4f`. The candidate is that base
plus this evidence document and `TestSettledCodexNamePublicationAndArchiveOnlyRead`;
the final commit is recorded in the containing PR. The test changes no production
code. Platform: macOS, Go `go1.27.1 darwin/amd64`.

The integrated test uses temporary archive/project/native homes, injected CLI
`Env`, and the existing in-memory object store. Native input and UUIDs are
synthetic. Hook registration, Codex filtering/parsing, settled-file label
interpretation, source/metadata/index publication, restart from durable local
state, and CLI list/show are production paths. The store is not S3/MinIO and
there is no scheduler or native app in this test. A hand-written sidecar tests
refusal; it does not reproduce a live SQLite writer. Native RPC tests use
synthetic transports, not a running Codex app-server. Claude records are fixtures,
not a CLI picker or producer run.

## Actual read-only observations

The orchestrator observed five existing Codex desktop sidebar entries through
the app's read-only thread API. Each native ID matched exactly one bounded
rollout `session_meta.id`, with producer `0.159.2`. Public aliases are
`codex-observed-1` through `codex-observed-5`; private labels, UUIDs, source paths,
and archive keys are deliberately omitted. Native origin fields were strings.
The default SQLite database existed with `-wal` and `-shm` sidecars. This is an
unavailable files lookup, not a settled-files parity observation.

An actual archive list attempt using source build `dev-bfbc1f4` failed with
Keychain `OSStatus -50`. One authorized read outside the sandbox produced no
output and was stopped with SIGTERM (exit 143), with its process handle released.
No archive metadata was read back. The installed earlier archive build was
`dev-f0dca28`. Installed Claude Code `2.1.273` help showed the naming/resume
surface, but no actual picker observation was available. CLI versions do not
establish the desktop build, which was not recorded. No disposable macOS account
or VM, Docker daemon, working disposable native app environment, or working
actual archive reader was available during this execution.

These observations establish a native UI-to-rollout identity baseline only;
they establish no archive match, mutation, coexistence, or native pagination.
The local redacted source-facts record and private identity map remain outside
source control. No screenshot or transcript is published here.

## Executed sandbox evidence

**PASS (synthetic pipeline only):**
`TestSettledCodexNamePublicationAndArchiveOnlyRead` verifies two admitted sessions
sharing one synthetic name, UUID-matched archived identity and distinct prompt
preview; a database-only rename with byte-identical transcript, unchanged
start/end/capture/counts/native records, and a changed retained source revision;
a reopened archive writer with zero remote writes on an unchanged pass; and
live-sidecar refusal preserving the last published name. After deleting the
synthetic native home, a fresh archive reader checks uncapped and limit-1 JSON
lists, default text list, and JSON/text show against the published name.
Native executable discovery and native-host startup fail the test if attempted.

**SYNTHETIC-COVERED (existing contracts):** Codex file-provider tests cover
legacy/index versus paginated naming, supported producer/storage guards, and
unavailable WAL/shape/relocation; native-label tests cover nullable names,
metadata-only wire requests, per-home identity, response/request limits,
deadlines and source compatibility. Collector history tests exclude ordinary
external-name authority even with a live ordinary source, then publish/read
history and settle across restart. Claude codec tests cover delayed generated
records, custom precedence over later generated records, and naming bookkeeping
without activity/admission changes. These are executable contracts, not native
producer/UI observations.

Run from the candidate checkout with Go 1.27.1, using a private writable cache
and the already populated module cache; `GOPROXY=off` prevents module downloads:

```sh
GOPATH=/Users/wangjohn/go GOCACHE=/private/tmp/agent-archive-comparison-evidence-cache GOPROXY=off \
  /usr/local/bin/go test ./internal/cli \
  -run '^TestSettledCodexNamePublicationAndArchiveOnlyRead$' -count=1 -timeout=60s
GOPATH=/Users/wangjohn/go GOCACHE=/private/tmp/agent-archive-comparison-evidence-cache GOPROXY=off \
  /usr/local/bin/go test ./internal/agents/codex ./internal/agents/nativecodec ./internal/collector \
  -run '^(TestLabels.*|TestNativeLabel.*|TestNativeCompatibility.*|TestClaudeNativeTitlePrecedenceAndOwnership|TestClaudeNameBookkeepingDoesNotChangeActivityOrAdmission|TestExternalLabelsExcludeOrdinaryActiveHistoryAuthority|TestRunNativeHistoryPublicationSettlesAcrossResumeAndRestart)$' \
  -count=1 -timeout=120s
```

Each command returned exit 0. Initial fixture development caught an event shape
without a parser prompt and the list JSON envelope; both fixtures were corrected
before recording this passing execution. Those failures were test construction
errors, not evidence of a production defect.

Focused `go vet` for the four packages and `go test ./internal/doclinks` also
passed. Linux-tagged scoped Levenshtein lint used the workflow-pinned
`b8b8ee429de80d44c00ae7c8020662de89f927c6` binary with a private
`STATICCHECK_CACHE`. An initial all-rules golangci-lint run reported 11
existing credentials-package exported-comment gaps. The repository explicitly
tracks this debt separately: its blocking full run disables revive, and its
revive run checks new code only (see `.golangci.yml` and the Test workflow).
The matching full `--disable=revive` gate and new-code-only revive run both
returned exit 0 using golangci-lint 2.14.0. No credentials code was changed to
make this evidence package pass. Hosted
platform tests and the full shared verify job remain CI gates, not local live
UI evidence.

## Protocol row disposition

All native UI rows remain **BLOCKED**. Synthetic coverage is recorded separately
and does not upgrade a row to PASS.

| Row | Executed substitute | Missing native acceptance evidence |
| --- | --- | --- |
| C1 / A1 | Integrated same-name identity and list/show publication; actual Codex sidebar-to-rollout baseline | Actual archive readback and native label equality; Claude picker identity |
| C2 / A2 | Integrated database-only rename; Claude bookkeeping fixtures | Native UI rename, signature observation and normal sandbox capture |
| C3 / A3 | Claude generated-title fixtures; Codex provider contracts | Initially unnamed native session then actual generated title |
| A4 | Claude custom-before-generated precedence fixture | Custom rename plus subsequent producer-generated record after work |
| C4 / A5 | Archive writer reopen and fresh reader | Native app/CLI restart and subsequent collector/readback |
| C5 / A6 | Integrated unchanged pass asserts zero remote writes | Unchanged native producer plus real publication evidence |
| C6 | Bounded synthetic metadata RPC contracts | Disposable desktop coexistence, responsiveness, auth/storage effects, process teardown |
| C7 | Integrated settled synthetic DB and sidecar refusal; actual sidecars observed | Settled native parity after normal shutdown and live refusal with archive readback |
| C8 | Integrated archive limit-1, default and uncapped list | Native UI scrolling/pagination and older UUID-matched row; >50 native list coverage |
| C9 | Existing storage-specific and history publication/restart contracts | Pre-existing native legacy/paginated cases and separate captured history source set |
| C10 / A7 | Deleted synthetic native home, fresh archive-only CLI list/show | Disposable native-origin published archive readback and process/file tracing |

Next execution needs a prepared disposable native environment and working
archive destination/reader. Follow the protocol for normal capture, native
mutations, UI identity and restarts; preserve each attempt's deadline and
fallback/refusal status. Do not checkpoint/remove real sidecars, copy auth,
manufacture ownership, or equate matching strings with publication evidence.
