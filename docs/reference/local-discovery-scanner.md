# Local discovery scanner

This phase adds disabled Codex source discovery machinery. It does not expose
setup activation or support additional agents; hooks and backfill remain the
available capture paths until the activation phase.

A scheduled collector observes approved active and archived rollout roots before
opening remote storage. A source scan has a five-second budget and at most 256 metadata
probes, with durable enumeration cursors and a bounded 8,192-entry metadata cache.
Up to 256 admission retries are retained separately from enumeration, with at
most 64 revisited per pass and half the probe budget reserved for forward
coverage. A conflicting source cannot pin a directory cursor; retry overflow
is reported and revisited by filesystem reconciliation.
Native date directories and compatible settled SQLite indexes prioritize work;
those hints never establish native creation time or authorize capture. Live WAL
indexes fall back to filesystem enumeration without creating native side files.

Every candidate needs supported metadata, a native first task, an included
project, and original creation within current project and destination consent.
Recognizable inherited or imported histories reject. A recent copy with otherwise
indistinguishable supported native metadata is eligible under the revised source
contract; file arrival is not proof that execution originated on this machine.
Old history remains old when copied or resumed.

Registrations remain authoritative. A missing identity lookup requests the
existing registration census, then defers admission until that census restores
an owner or records explicit absence. Census enumeration and source reads run
outside the hooks lock. A completed census also checks bounded directory presence:
if both derived index directories are empty while registrations remain, it
rebuilds ownership before scanning, including when startup recreated the empty
directories. This preserves admitted continuations outside a newer start window
without requesting recovery for every old unknown source. Arbitrary individual
index loss while other entries survive still requires a recovery signal.
The census currently enumerates registrations in memory;
it is deadline-aware but has no fixed entry or memory cap. Recovery retains the
collector pass context rather than the scanner's five-second deadline, so a
larger census can finish instead of repeatedly restarting and blocking admission.
This phase does not
claim universal two-pass latency or release-machine resource acceptance.

Collector publication reopens discovery files within their approved root and
checks native identity, original creation, working directory, and admitted
producer version, originator, and execution source against the
registration before filtering. Existing origins, destination attribution,
admission time, and removal records are preserved. Synthetic tests inject adapter
support within the scanner package; there is no production support override.

## Measured operating envelope

A synthetic Linux Go 1.27.1 run exercised 1,000, 10,000 and 100,000 source files
in flat, native-date, settled-index and live-WAL layouts. Each case ran once;
passes ran consecutively to measure work, without scheduler sleeps. Fresh bursts
in these warmed fixtures required two scheduled recovery-before-scan stages.
Measured accumulated task time was 97–456 ms. This is a measured fixture result,
not a guarantee for all directory layouts, histories, cache states or machines.
The bounded cache still causes historical metadata rereads: the filesystem warm
cases probed 50–256 headers in the measured first stage.

Cold filesystem coverage required 4, 40 and 392 stages at those sizes. Settled
indexed coverage required up to 578 stages at 100,000 files in the dated layout.
The default Linux and macOS schedules run every 60 seconds. Thus 392 or 578
stages imply roughly 6.5 or 9.6 hours of scheduled cold coverage, despite only
54–57 seconds of accumulated work in the 100,000-file filesystem benchmarks.
Manual runs or changed schedules have different elapsed times. Source hints
prioritize recent work; they do not promise immediate exhaustive cold coverage.

A fresh unknown identity also needs a complete authoritative registration
census. With 1,000, 10,000 and 100,000 existing registrations, the measured
recovery-plus-admission task took 82 ms, 543 ms and 5.33 seconds respectively.
The 100,000-registration task allocated 1.125 GB in total across its execution;
process maximum resident memory at that point was approximately 82 MB. Total
allocation and process high-water memory are different measurements, and these
results do not establish release-machine resource or isolated lock acceptance.
Census work is outside the five-second source-scan budget and runs before
publication, so large inventories can delay publication even when source
observation itself is bounded. This implementation retains the existing census
architecture rather than asserting a fixed memory or time limit for it.

## Blanket policy integration

Codex scope is independent of the ingress. All mode shares a single consent
window across physical projects; discovery additionally checks its own approved
source window. Resolving a candidate never saves configuration. New projects
remain separate registration/archive identities, even below a configured parent.
Fresh hook starts use the same permission windows, while retained hook intents
replay their original observation and freshness evidence. Neither resuming a
session nor finding its file later creates a new historical permission.

Physical resolution has a fresh pass-local cache of at most 1,024 cwd facts,
64 ancestors, 4 KiB per Git metadata file, at most 1,024 accounted metadata
operations and 256 KiB aggregate Git bytes. Canonicalization counts each
`lstat`/`readlink`; other counted operations include directory checks and Git
metadata opens/stat/read work. These are resolver limits, not total scan I/O.
Exhaustion retains an admission retry with `project_budget_exhausted`; it never
invents a nonGit fallback for an unresolved Git/worktree mapping. Cache facts
expire each pass, and every reuse validates the observed file/directory
identities, missing Git markers and regular-file fingerprints within the same
aggregate budget. Metadata-operation counts cover explicit lstat/readlink,
open/fstat and bounded metadata-file read calls, rather than hidden
canonicalization stats or transcript/index I/O. Source probe, directory cursor and rotating retry budgets
remain unchanged, and complete identity census still governs unknown IDs.

`discovery-health.json` is an atomically written, versioned summary capped at
16 KiB. It contains aggregate counts, stable diagnostic codes and attempt/
reconciliation timestamps, with no cwd, project sample, native ID or transcript
body. Status reads this summary without opening the full source catalog, Git,
SQLite or storage. Missing/corrupt/oversized summaries are uncertainty; freshness
must be evaluated against the caller's clock. Scan evidence remains distinct
from registration, queued publication, upload and verified readback.
