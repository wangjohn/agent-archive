# Integration performance reference

Production reference: `98f8bd0898abc8f22e4d67d567cf11ae2b88ee2f`.
Candidate base: `c85d239f09a3ae963f59e7023153a7a7f3ee7539`.
Both run on this Linux amd64 host (Xeon Platinum 8573C, Go 1.27.1,
benchmark GOMAXPROCS 5). The baseline uses test-only instrumentation from this
phase: the three new benchmark files and expanded `scripts/measure-hook.py`,
not production patches. All content is synthetic, with temporary data homes,
injected repository lookup and fake object storage. Raw results are in
`agent-integration-baseline-data/`. Candidate comparisons/checks are pending;
this draft does not declare the phase-entry performance gate complete.

## Reproduction

Create a detached checkout at the exact reference above; copy
`internal/capture/hook_bench_test.go`,
`internal/collector/settled_scan_bench_test.go`,
`internal/archive/analysis_consumers_bench_test.go` and
`scripts/measure-hook.py` from this phase into it. Preserve and record their
SHA-256 hashes with each comparison. Run both revisions serially on the same
host/toolchain, outside correctness/lint compilation or other benchmarks:

```sh
go test -run '^$' -bench 'BenchmarkScan(SettledRegistrations|Large)' -benchtime=1x -count=3 ./internal/collector
go test -run '^$' -bench BenchmarkHookLocalEffects -benchtime=10x -count=3 ./internal/capture
go test -run '^$' -bench 'BenchmarkAnalysisConsumers|BenchmarkCursorTextFilterFiveMegabytes' -benchtime=10x -count=3 ./internal/archive
go test -v -run 'TestLargeRecordMemoryCeiling|TestCursorSQLite|TestParserUpgradeOverAnUnchangedTranscriptDoesNotReadIt|TestParserUpgradeWithNewContentReadsTheTranscriptOnce|TestCursorText.*Limit|TestRecordOverTheLimit' ./internal/archive ./internal/collector
go build -o /absolute/temporary/path/agent-archive ./cmd/agent-archive
TMPDIR=/var/tmp python3 scripts/measure-hook.py /absolute/temporary/path/agent-archive
```

This managed sandbox adds a read-only `.git` sentinel to `/tmp`, causing the
real CLI data-home guard to refuse that location. Subprocess measurements used
an approved isolated `/var/tmp` run. The guard was not patched or disabled.
The subprocess harness creates private HOME/AWS paths, never installs hooks or
contacts storage, and checks that synthetic registration/candidate effects
occur before timing. Subprocess fresh is Claude; local fresh is Codex.

## Initial baseline

Median of three benchmark samples, not an ordinary CI wall-clock assertion:

| Scenario | Runtime | Bytes/op | Allocs/op | Work |
| --- | --- | --- | --- | --- |
| 1k settled registrations | 27.87 ms | 5,519,840 | 43,367 | 0 filters, 0 published bundle decodes |
| 10k settled registrations | 285.09 ms | 57,361,696 | 430,769 | 0 filters, 0 published bundle decodes |
| Large unchanged file with hook request | 274.27 ms | 39,694,592 | 194,979 | Deliberate full filter/compare |
| Large growing file | 349.99 ms | 70,002,648 | 295,618 | One changed source |
| Labels + metadata + transcript + handoff | 86.34 µs | 7,704 | 108 | Four parser entry calls today |
| Five-MiB Cursor text filter | 269.48 ms | 48,125,896 | 61,356 | Whole text record |

32-MiB large dropped-field record: sampled peak heap **128.0 MiB** above
baseline, **160.1 MiB** total allocated, 358 bytes retained. Sampled peak is not
an exact maximum. Limit/refusal fixtures also passed; database native values
are currently materialized before collector size validation.

## Counted gate inventory

| Proposal scenario | Executable evidence / preserved exception |
| --- | --- |
| Disabled/paused/ignored | `BenchmarkHookLocalEffects`: zero repository probes; actual pipeline has no source pass/database/version operation. New registry must preserve this before lookup construction. |
| Fresh/stop/subagent | Local-effect allocations and subprocess distributions; existing `hook_registration_race_test.go`, `admission_generation_test.go`, `hook_lock_wait_test.go`. New migration must eliminate current corrupt-index registration scan from hooks. |
| 1k/10k unchanged | `BenchmarkScanSettledRegistrations`: zero filters, bundle decodes and remote Get; existing `TestUnchangedSessionsCostNoWritesAndStayFast` pins local writes. |
| One changed amid unchanged | New `TestOneChangedFileAmidSettledSessionsFiltersOnce`: 32 sessions, 31 skips, one filter/publication, zero remote Get. |
| Parser-only upgrade | `TestParserUpgradeOverAnUnchangedTranscriptDoesNotReadIt` and `TestParserUpgradeWithNewContentReadsTheTranscriptOnce`: no unchanged native filter, once if changed. |
| Many Cursor chats | `TestCursorSQLiteOneSnapshotPerPass`: four changed chats, one copy; Cursor reader tests cover failed attempts and cleanup. |
| Large/near-limit | `TestLargeRecordMemoryCeiling`, record/text limits and five-MiB tests; ordinary allocation bounds and refusal semantics retained. |
| Deterministic/transient failure | `TestCursorSQLiteFailuresCostNoCopies`: missing/unsafe/size across three passes; cursorstore lock/changed-read fixtures preserve retry failures. |
| Multiple derived consumers | `BenchmarkAnalysisConsumers`: current repeated traversal baseline. One Analysis is a future counted phase-5 gate, not current behavior. |

Ordinary unchanged scans differ from deliberate file hook requests, which
refilter even when stat matches. Cursor text is always reread while present,
because stat equality cannot prove unchanged text; parser-only rederivation
can reuse safe published evidence. Do not apply the zero-filter gate to these
exceptions. Cursor signatures read per-chat native rows in place and never use
global database mtime or force a snapshot for unchanged-only passes.

Current architecture has no integration registry/pass/Analysis instrumentation.
Their operation counters must be introduced alongside real callers in later
phases, rather than claimed proven here. Relative >10% latency/peak regressions
require isolated reproduction and correction/justification; existing hook
absolute lock/read budgets still apply. macOS and real-systemd checks require
CI; this Linux reference cannot claim them locally.
