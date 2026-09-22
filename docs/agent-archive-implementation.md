# Agent Archive implementation and verification ledger

## Current remediation status

See [the eight-PR acceptance record](agent-archive-remediation-acceptance.md)
for current implementation evidence and remaining capability/live gates.
The historical notes below are retained for context and are superseded where
that record differs. In particular, first-seen Cursor starts are no longer
assumed fresh, and skill eligibility cannot be inferred from installation.

## September 21 audit follow-up

The historical progress notes below describe earlier slices. The following PRs
address the first implementation audit; they are open for review, not merged.

| PR | Change | Base |
| --- | --- | --- |
| [#24](https://github.com/wangjohn/agent-skills/pull/24) | Exact pending bytes, hook evidence retention, safe request acknowledgement, fixed capture boundary and rewrite protection | main |
| [#25](https://github.com/wangjohn/agent-skills/pull/25) | Parser-only metadata regeneration from retained source; preserve richer unpublished candidates | #24 |
| [#26](https://github.com/wangjohn/agent-skills/pull/26) | Bounded skill observations and version history, explicit feedback, filtered hook-only final/model fields | #25 |
| [#27](https://github.com/wangjohn/agent-skills/pull/27) | Separate read-back evidence, configuration identity, authentication freshness, capture diagnostics, transactional prototype job migration | #26 |
| [#22](https://github.com/wangjohn/agent-skills/pull/22) | Current/predecessor retention and metadata-first expiry with interruption recovery | main |
| [#23](https://github.com/wangjohn/agent-skills/pull/23) | Fail-closed signing and accepted notarization gate for tagged releases | main |

Review/merge the stack in order #24 → #25 → #26 → #27. Retention and release
changes are independent. Retarget dependent PRs to main as their bases merge.

The combined stack passed `go test -race ./...`, `go vet ./...`, repository
skill validation, all 15 Python prototype tests, and the release-gate regression.
Both Intel and Apple Silicon development executables built; Intel version/help
commands ran on this host. ARM runtime execution was not available on this host.
These were synthetic tests. No private session was uploaded and no real app
hooks were installed for validation.

Live acceptance remains separate: synthetic R2 round trips first; AWS is deferred
by the user. Actual app trust/capture, launchd and background Keychain behavior,
a second physical Mac, measured end-to-end hook latency, and Apple-signed and
notarized downloads remain unverified. Filesystem inventory is explicitly
installed-only evidence, with coverage gaps for uninspected roots/plugins.
Evaluate Skill recommendations remain separate work as specified in the plan.


Source of requirements: [engineering specification](agent-run-archive-spec.md).

Status: implementation in progress. Passing synthetic tests does not establish live application or cloud compatibility. Entries remain incomplete until reviewed evidence exists.

## PR boundaries

| Slice | Requirements | Evidence required | Status |
| --- | --- | --- | --- |
| Foundation | Versioned metadata/source schemas; small module; fixture corpus | Schema examples, synthetic parser fixtures | In progress |
| Capture | Activation cutoff, project inclusion, safe native filtering, source provenance, size limits, unknown fields, model/skill gaps | Secret, hidden-content, old-session, incomplete-line tests | In progress |
| Snapshots | Deterministic bytes/hash, timestamps, parser regeneration, hook message reconciliation | Unchanged scan and source-preservation tests | In progress |
| Storage | R2/S3 SDK, explicit profile, Keychain, integrity, write-before-pointer, synthetic permissions probe | Fake endpoint + real provider round trips | In progress — code complete and now wired end to end through `agent-archive setup`, which calls `VerifyAccess` against whatever the user configures. Only an actual live R2/S3 credential exercising that path is unverified |
| Collector | Registration, hook-only evidence, scan cadence, queue, lock, retries, delayed final, compaction, disk failures | Crash/restart/ownership tests | In progress (PR in review)\*\* |
| CLI | setup/status/sync/pause/resume/help/version; actionable errors | CLI scenario tests | Done (PR #8) — `cmd/agent-archive` plus `_hook`/`_collect`/`status`/`sync`/`pause`/`resume` all implemented and tested; `setup` was a stub in #8, now real (see below); read-only `list`/`show` added later (see the Reader row) |
| Setup | Existing bucket, hidden secrets, app selection, projects, consent, no history import, activation preservation | Reconfiguration/cancel/rollback tests | Done (PR #9)\*\*\* |
| Hooks | Three harnesses, preserve unrelated hooks, trust remains explicit, prototype migration | Merge/idempotency tests, live lifecycle tests | In progress (PR #5, wired up in #8/#9) — config merge/install/rollback done and tested, and now actually invoked by `_hook`/`setup` instead of only existing as generated strings; live install against a real harness is still unverified\*\*\*\*\*\* |
| Scheduling | Login LaunchAgent, absolute runtime path, background auth, persistent pause | Plist and fresh-process tests | In progress (PR #5, wired up in #9) — plist generation, writing, and a `launchctl bootstrap` load attempt are now all invoked by `setup`; a real `launchctl`/launchd round trip remains unverified (not testable in this environment) |
| Metadata | Harness/version/settings; requested vs response model; skills installed vs discovered; evidence and gaps; counts | Mixed-model/no-skill fixtures | Done (PR #6) |
| Reader | Metadata filtering, selected-source download, hash verification, on-demand normalized view, feedback provenance | Read-back and filter tests | Done (PR #6)\*; exposed on the CLI as `agent-archive list`/`show` (see the note) |
| Retention | Current/predecessor, grace period, whole-session deletion, same-machine ownership, pending-pointer safety | Expiry/race tests | Done (PR #10)\*\*\*\* |
| Distribution | Intel and ARM executables, checksums, signing/notarization, docs | Build artifacts and release workflow | In progress (PR #11)\*\*\*\*\* — workflow, build script, and docs land buildable and reviewable now; an actually signed and notarized artifact needs the Apple credentials this environment doesn't have |
| System verification | Two Macs, two providers, each installed harness; enabled/disabled latency | Live smoke records, timing report | Pending external access |

\* Reader covers metadata filtering (harness, model, skill, coverage, eligibility), verified bounded source reads, and refresh-on-deletion recovery, each with tests. It does not yet surface individual explicit-feedback items with their own provenance to readers; `Metadata.Counts.ExplicitFeedback` is a count only. Closing that gap is follow-up work, not blocking. The CLI surface over it is `agent-archive list` (`--harness`, `--model`, `--skill`, `--skill-usage`, `--since`, `--complete`; metadata sidecars only, via `reader.ListMetadata`) and `agent-archive show <archive-session-id>` (the metadata sidecar as JSON; conversation content — the normalized view from `archive.ParseNormalized` over a `reader.RefreshAndLoad`-verified bundle — only with an explicit `--normalized` flag, per the spec's rule that nothing prints transcript contents by default). Both open the configured store the same way a collector pass does, take no lock and ignore pause since they only read, and report "not set up" the same way `status` does; `internal/cli/inspect_test.go` covers them against the in-memory store.

\*\* Builds on PR #5's `internal/local` (private home directory, atomic file I/O, flock-based machine lock) rather than re-implementing them; adds the scan/build/publish loop, change detection against a local per-session cache (so an unchanged transcript costs no storage write and never gets a new `captured_at`), and a per-session minimum upload interval. Does not yet implement snapshot cleanup/retention or a parser-upgrade-only republish path — both explicitly deferred to the Retention slice and a later pass, respectively. (PR #9 added `Options.RequireSkillUse`, a third, distinctly non-retriable cache status alongside "published" and "rate-limited" — a session policy-declined for having no detected skill use is reconsidered only by an actual further content change, never by time alone.)

\*\*\* Detects applications by checking for the same `.codex`/`.claude`/`.cursor` directories `hooks.Plan` already targets (best-effort: existence is not proof of a current install, and the reverse); prompts for storage, applications, and projects, preserving existing project activation times and stored R2 credentials on reconfiguration; and only leaves anything behind (Keychain secrets, hooks, LaunchAgent plist, `internal/config`) once setup actually finishes, so a decline or a failure at any earlier step leaves a prior configuration untouched. Two exceptions this required deliberate rollback for, both found by `review-pr` and fixed rather than left as gaps: an R2 secret has to be saved to Keychain before `VerifyAccess` can read it back to probe the bucket, so a verification failure (or anything after it) now deletes that freshly-written secret again, never touching a reused existing one; and hooks/the LaunchAgent are installed before the final `config.Save`, so a `config.Save` failure now unloads and removes the LaunchAgent and rolls back the hooks, rather than leaving both live with no config behind them. The `launchctl bootstrap`/`bootout` calls and installed-application detection are both real but unverified against a live macOS install — this environment cannot run them for real, only exercise the code paths with fakes.

\*\*\*\* `internal/retention.Sweep` runs as an additional pass after every successful `collector.Run` (wired into `_collect`/`sync` via `internal/cli/collect.go`), driven by an append-only local ledger of superseded (no-longer-current) source keys recorded in `internal/collector/lineage.go` whenever a republish changes a session's current source — rather than a single-predecessor-slot pointer, so rapid successive republishes before a sweep ever runs cannot leak an untracked orphan; a key that is superseded again after content reverted to it moves to the end of the ledger, so append order is always supersession order. Before deleting anything the sweep fetches the session's remote `metadata.json` and validates it as the current pointer (it fails closed, preserving every snapshot, when that metadata is missing, unreadable, or belongs to another session). The currently referenced source is never a candidate regardless of age, and neither is its immediate predecessor (the most recently superseded snapshot); any older superseded source is deleted once it has been superseded for longer than a 24-hour grace period. The remote fetch is skipped when no ledger entry other than the newest is past its grace period and the session is not locally past its retention age, so idle sessions cost no round trip. Whole-session deletion triggers once a session's most recently captured evidence — the later of the local cache and the remote metadata's capture time — is older than `Config.RetentionDays` (90-day default), deleting the metadata pointer before its source snapshots, so an interruption partway leaves at worst unreferenced objects for the next sweep, never a live pointer to missing data; a failed expiry leaves the local registration (ownership) in place so the same machine retries it. Only the machine that registered a session ever sweeps it, since registrations are local-only. One session's sweep failure is isolated and left retryable on the next pass, matching `collector.Run`'s own error-isolation model.

\*\*\*\*\* `.github/workflows/release.yml` triggers on a `vX.Y.Z` tag push, cross-builds both `darwin/amd64` and `darwin/arm64` from one `macos-14` runner via `scripts/build-release.sh` (the same script a maintainer runs locally, so CI and a local build can never drift), embeds the release version with `-ldflags -X .../internal/cli.Version=...`, and verifies the embedded version before proceeding. Codesigning (import a Developer ID Application certificate into a temporary keychain, `codesign --options runtime --timestamp`) and notarization (`xcrun notarytool submit --wait` against a zip of each binary, since notarytool doesn't accept a bare executable) require `APPLE_SIGNING_ENABLED=true` and all signing secrets. Tagged releases now fail closed when signing is unavailable; both notarization submissions must report `Accepted` before publication. Local development builds remain unsigned. `docs/install.md` documents download-and-verify, building from source, what signing currently requires, and removal via `agent-archive uninstall` (with the manual recipe kept as a fallback). None of the signing/notarization path can be exercised for real in this environment — no Apple Developer credentials are available here — so it is implemented and reviewable but unverified, the same status the ledger already gives Hooks/Scheduling for their own live-environment gaps.

\*\*\*\*\*\* `_hook`'s SessionStart handling declines to register a never-seen native session whose payload says it continues an earlier conversation, since that conversation's true start time cannot be established and the spec excludes older resumed sessions by default. This originally applied only to Claude Code's `"source":"resume"`; Codex's hook documentation (https://learn.chatgpt.com/docs/hooks.md, "Common input fields" and SessionStart) now documents the same `source` field with values `startup`, `resume`, `clear`, `compact`, so the check covers Codex too. The rule is now positive rather than a resume blocklist (see `docs/agent-archive-session-eligibility.md`): a never-seen Codex or Claude Code session is registered only when `source` is `startup` or `clear`; a missing, unknown, `resume`, or `compact` source leaves it uncollected, and `compact` (or `resume`) of an already-registered session keeps its original start time and transcript path, matched by project identity (a continuation reported from a subdirectory of the registered project is the same session; a different harness or a different configured project is rejected). Cursor documents no equivalent start-provenance signal, so a never-seen Cursor start is also left uncollected rather than assumed fresh; that is a capture limitation pending live verification, not a claim of Cursor support. For included projects each declined start leaves a content-free local diagnostic that `status` reports and that is pruned when the project is later excluded.

Evaluate Skill authoring and controlled evaluation runner are explicitly separate work in the engineering spec. This implementation must provide their reader/data interface, not silently omit it or claim the evaluation skill itself exists.

## Remaining PR sequence to completion

With PR #7 (Collector) in review, this is the full remaining sequence to close every Pending or partially-done row above, in dependency order. Each PR is scoped to what the spec's own component boundaries and this repo's existing PR granularity suggest; none of it needs to start from zero; the code inventory below is what each PR builds on, not what it still has to write.

**PR #8 — CLI foundation — done, in review.** `cmd/agent-archive`, `internal/cli`'s `_hook`/`_collect`/`status`/`sync`/`pause`/`resume`, and `internal/config` all landed as described below.

**PR #9 — Setup — done, in review.** `agent-archive setup`'s guided flow landed as described below; it also closed the Storage row's live-round-trip gap by being the thing that actually calls `VerifyAccess` with real configuration.

**PR #10 — Retention — done, in review.** `internal/retention` and its `_collect`/`sync` wiring landed as described above.

**PR #11 — Distribution — done, in review.** `.github/workflows/release.yml`, `scripts/build-release.sh`, and `docs/install.md` landed as described above. Producing an actually signed, notarized artifact remains blocked on the Apple credentials already listed below as unavailable to this environment.

**Not a PR — System verification**
Two Macs, two providers, each installed harness, and an enabled/disabled latency comparison. This is a live-access milestone gated on the same external prerequisites listed below, not code to write.

**Ongoing, not a dedicated PR — Foundation, Capture, Snapshots**
These keep hardening opportunistically as new harness versions or edge cases surface (every PR so far has touched at least one of them alongside its main deliverable) rather than needing a PR of their own.

**Out of scope — Evaluate Skill authoring and its benchmark runner**
Explicitly separate work per the spec. The Reader slice (PR #6) already provides the data interface it will need; no PR for the skill itself belongs in this sequence.

**Correction:** an earlier version of this section recommended Collector as "the next PR" without having read `internal/hooks` or `internal/local`, and so incorrectly described Hooks and Scheduling as having no code behind them. The first draft of the Collector implementation also duplicated `internal/local`'s atomic-write and lock logic with a weaker approach (a PID-staleness heuristic instead of a real `flock`) before this was caught and fixed to build on `internal/local` directly. This note exists so the mistake doesn't get repeated silently.

## Current external verification prerequisites

- Cursor is not installed on this Mac; Cursor fixture coverage cannot be labeled live-verified.
- R2/S3 test destinations and credentials are pending user configuration. Do not send private sessions to a test endpoint.
- Apple signing/notarization credentials and the second Mac are not yet available to this run.

## Review policy

Root reviews each implementation diff and test evidence before a PR becomes merge-ready. At most two implementation agents run, each in an isolated worktree. Dependent PRs target their prerequisite; independent slices share only reviewed contracts. Never include the old uncommitted Python prototype accidentally.

## Setup and CLI refinement (September 2026)

The [CLI plan](agent-archive-cli-plan.md) is implemented in the existing Go
module: centralized help/argument preflight, three-step setup with non-secret
resumable drafts and hidden terminal input, staged Keychain references, a durable
installation recovery journal, human/JSON status, and data-preserving uninstall.
The collector's existing source checksum read-back supplies publication evidence;
status does not infer trust or complete coverage from configuration.

Setup preserves pause, identity, and existing activation times; removed apps lose
only owned hooks. Destination switches reject pending work and retire old sessions
from subsequent collection/cleanup while keeping local evidence and destination
references. Retention reductions preview owned sessions before confirmation.
Pause uses the same machine lock as collection and asks for a retry if a pass is
still in flight. No operation is reported paused while that pass still runs.

Automated scenarios cover interruption/resume, provider setup, credential isolation,
rollback and crash recovery, changed-file protection, project path aliases, command
help, observed status, destination boundaries, and uninstall with unknown files.
Live cloud credentials, real app trust, launchd transitions, two-Mac operation, and
signed/notarized release verification remain external checks. The Python recorder
is explicitly a legacy prototype, not the runtime for the new CLI.

Local verification for this refinement:

- `go test -race ./...` passed, followed by targeted CLI/control regressions
  after final changes; `go vet ./...` passed.
- Skill validation and all 15 legacy Python tests passed.
- `scripts/build-release.sh` built unsigned `dev` binaries for macOS Intel
  and Apple Silicon; both generated SHA-256 checksums verified.
- The native built executable passed all eight command-help smoke checks,
  invalid-argument handling, and unconfigured JSON status. None created an
  archive data directory.
- A pseudo-terminal test verified secret input disables terminal echo and
  restores it afterward. It used a synthetic secret and no Keychain/cloud access.

The final onboarding pass offers the current Git project with explicit consent,
reads existing AWS profile names and regions without resolving credentials, and
moves optional choices into the final review's Edit menu. Non-storage edits reuse
the successful access check; changed storage is checked again. Regression tests
cover the short path, manual project fallback, profile switching, provider help,
review edits, cancellation/EOF, and home/symlink normalization. The full Go race
suite, Go vet, skill validation, and 15 legacy Python tests passed; the final CLI
regressions and both unsigned macOS builds were also verified.

## PR A1 — Session registration

Three registration defects, all in `internal/cli`.

**Worktrees and subdirectories.** `handleSessionStart` required the hook `cwd`
to equal a configured project root exactly, while continuations already
resolved the owning project. A Claude Code session in
`<project>/.claude/worktrees/<name>`, or in any subdirectory, therefore never
registered, left no diagnostic, and made its later `Stop` a no-op. New
registrations now resolve the owning project with the same nearest-configured-
ancestor rule, register under the configured root spelling, and derive the
project ID, eligibility, and activation check from it. The nearest ancestor
wins, so a configured project nested inside an included one keeps its own
exclusion. A working directory inside no configured project stays silent.
`config.AcceptSession` needed no change: registrations already store the
configured spelling it compares against.

**Cursor fresh starts.** `provesFreshSessionStart` accepted only a Codex or
Claude `source` of `startup`/`clear`, so no Cursor session could ever register.
A second, harness-independent proof was added: the hook-provided
`transcript_path` names a file that is absent or empty, which only a
conversation that has not happened yet can do. A transcript with bytes is a
resume and keeps the existing `session_start_unknown` diagnostic. The proof
applies to Cursor, and as a fallback to Codex and Claude when `source` is
missing; a present `source` still decides. A start with no `transcript_path`,
or a transcript that cannot be stat'd, proves nothing. The transcript is never
opened and its path never reaches a diagnostic. Cursor's `fresh_start`
capability moves from `unavailable` to `documented`, citing `transcript_path`
rather than `cursor_version`, and `status` now asks for the new session that
would prove capture instead of declaring it impossible.

**Setup-in-progress starts.** `handleHookEvent` returned silently while a setup
transaction was open. It now records a content-free `setup_in_progress` capture
diagnostic for included projects, matching the pre-activation and unknown-start
cases. The write is lock-free and best effort because setup holds `hooks.lock`
while it commits; a harness's turn is never delayed behind an installation.
Only session starts record it, and excluded or unconfigured paths still record
nothing.

Tests (`internal/cli`): worktree and subdirectory starts register under the
parent project and their `Stop` produces a request; a nested excluded project
is not captured through its parent; a sibling directory outside every project
is silent; the Cursor proof across empty, not-yet-created, non-empty, and
absent transcript paths; Codex and Claude across `resume`, `compact`,
`startup`, and missing `source` with empty and non-empty transcripts; the
`setup_in_progress` diagnostic in `status --json` and its absence for
unconfigured paths and non-start events. `go build`, `go vet`, `gofmt -l`, and
`go test -race ./...` are clean.

## PR B1 — Privacy filter v3

Filter version `3`, adapter version `0.3.0`. Metadata and source schema
versions are unchanged; `nativeEvidenceExtends` already treats a version change
as not-a-rewrite, so republishing after this upgrade is safe and older bundles
stay readable at their recorded filter version.

Four changes in `internal/archive/adapters.go` and `internal/evidence/skills.go`:

1. **Tool evidence is retained.** The key allowlist is no longer applied inside
   a tool-argument subtree (`input`, `arguments`, `tool_input`, and Codex's
   `payload.input`), so Edit `old_string`/`new_string`, Agent `prompt`, Skill
   `args`, Grep `pattern`, Bash `timeout`, and MCP arguments survive. Added to
   the allowlist: `tool_use_id`, `is_error`, `stop_reason`, `usage`,
   `sessionId`, `requestId`, `gitBranch`, and the Codex token-accounting keys
   `info`, `total_token_usage`, `last_token_usage`, `turn_token_usage`,
   `thread_token_usage`, `last_agent_message`, `thread_id`, `root_turn_id`,
   `started_at_ms`, `completed_at_ms`. `usage` and the four `*_token_usage`
   subtrees retain numbers only. New record types: Codex `token_usage_record`
   and Cursor `turn_ended`. `toolUseResult` stays excluded as duplicate
   content. Value policy is unchanged everywhere.
2. **Omissions are visible.** The per-field `unknown_field_omitted` gaps
   collapse into one gap whose detail lists the distinct omitted key names,
   sorted and capped at 64. Names only; a truncated list says so.
3. **Injected instructions are stripped.** `<system-reminder>`,
   `<user_instructions>`, and `<environment_context>` blocks are removed from
   string content with a `hidden_instruction_omitted` gap; an unterminated
   block drops everything from its opening tag. The rest of the message stays.
4. **Skill snapshot bodies are capped** at 16 KB with `original_bytes`
   recorded; the inventory and the hash of the whole original file are
   unchanged. Content-addressed snapshot objects remain deferred.

Fixtures (synthetic) under `internal/archive/testdata/`:
`claude-tool-evidence.jsonl` (assistant Edit `tool_use` with `usage` and
`stop_reason`, user `tool_result` with `tool_use_id`/`is_error` plus a
`toolUseResult`, user prompt with a `<system-reminder>`),
`codex-tool-and-usage.jsonl` (user message with `<user_instructions>` and
`<environment_context>`, `custom_tool_call`, `custom_tool_call_output`,
`token_usage_record`), `cursor-turn.jsonl` (`{role, message:{content}}` user
and assistant records and a `turn_ended` record).

Tests in `internal/archive/filter_v3_test.go` cover each fixture's retained and
dropped keys, a secret inside an Edit `new_string` still being redacted, a
blocked key and the 64 KB cap still applying inside tool arguments, the omitted
key names appearing once and capped without leaking values, unterminated and
whole-message instruction blocks, and the declared capture provenance.
`internal/evidence/skills_test.go` now asserts the 16 KB body cap and
`original_bytes`.

Local verification: `go build ./...`, `go vet ./...`, `go test -race ./...`,
and `gofmt -l .` all clean. `internal/cli`, `internal/collector`,
`internal/reader`, and `internal/retention` needed no changes for the version
bumps. Parser work (Cursor turns, corrected counts, tool linkage in the
normalized view) is PR B2 and is not in this change.

Review additions (same PR, same filter version, before any bundle was written
with filter 3):

- Instruction-block stripping is now depth-aware. The regex form ended a
  `<system-reminder>` at the first closing tag, so a block nested inside a
  block of the same kind leaked the outer block's tail. Blocks are now scanned
  to their matching close, counting nesting; an unterminated block still drops
  everything after its opening tag.
- Tool-argument deny list: `text`/`value`/`values` are dropped for typing and
  form-submitting tools (`type`, `form_input`, `computer`, `key`,
  `enter_verification_code`, `autofill_credential`, MCP names ending in one of
  those, or names ending `_type`/`_input`/`_fill`), and any argument whose key
  contains `password`, `secret`, `token`, `credential`, `api_key`, `apikey`,
  `cookie`, or `authorization` is dropped for every tool. Key names are
  reported once in a `sensitive_or_hidden_field_omitted` gap.
- Value-level redaction now also covers PEM private key blocks, JWTs, URL
  userinfo, GitHub tokens, and Slack tokens. The assignment pattern's known
  false-positive class (`token = parse(x)`) is documented, not narrowed.
- Tests in `internal/archive/filter_v3_review_test.go` cover nested and
  multiple blocks, the deny list for Claude and Cursor shapes and for
  supplemental evidence, each credential shape, a negative set of ordinary
  code, and byte-identical output across repeated scans of each fixture.
- `docs/install.md` no longer describes filter 2 / adapter 0.2.0 as current.

## PR B2 — Parser v0.6

`DefaultParserVersion` is `0.6.0`. Metadata schema version 1 is unchanged: the
new counts are optional fields, and metadata written by an older parser stays
valid. Stacks on PR B1; a bundle captured under filter 2 still parses, with the
fields this parser added simply absent.

1. **Cursor sessions no longer derive zero turns.** `visibleMessage` now also
   reads a record which carries `role` at the top level and `content` under
   `message`, which is Cursor's shape. `turn_outcome` and the lifecycle state
   come from a native `turn_ended` record when the hook evidence left them
   unknown; an observed hook stop, interrupt, or closure still wins.
2. **Counts mean what they say.** `counts.turns` counts human prompts (a user
   record with text or any non-tool-result content), `counts.messages` counts
   those prompts plus assistant records — including assistant records whose
   only content is a tool call — and the new `counts.tool_results` counts
   observed tool results. A user record carrying only `tool_result` blocks was
   previously counted as both a message and a turn, which is what made a
   four-prompt session report twenty-eight turns. A parsed structured bundle
   now reports a known zero instead of leaving a count unknown.
3. **Codex tool calls are recognized.** `custom_tool_call`, `local_shell_call`
   and `item_completed` items of type `CommandExecution`, `McpToolCall` and
   `Extension` join `tool_use`, `tool_call` and `function_call`. An
   `item_completed` whose call was already reported by its own record is
   dropped, keyed on `call_id`/`item.id`, so the same work is counted once.
4. **The normalized view carries tool evidence.** `NormalizedToolCall` gains
   `name`, `input` (the retained argument object, decoded when a harness
   encodes it as a JSON string), `result_record_index`, `is_error` and
   `output_bytes`, linked by `tool_use_id` (Claude), `call_id` (Codex), or
   position (Cursor). Position is used only when no call in the bundle carries
   an identity; elsewhere an unmatched result stays unlinked rather than being
   attached to the wrong call. `NormalizedView.ToolResults` lists the results
   themselves, and `show --normalized` prints both.
5. **Token usage reaches metadata.** `counts.input_tokens`,
   `output_tokens`, `cache_read_tokens` and `cache_write_tokens` are summed
   from Claude `message.usage` and Codex `turn_token_usage` (cumulative and
   thread-wide figures are ignored so the sum stays additive) and stay nil when
   the harness exposed nothing.

Fixtures: the B1 fixtures are reused; `codex-tool-events.jsonl` (turn context,
`local_shell_call`, and `item_completed` events of each recognized item type,
one of them an echo of the shell call) and `claude-tool-only-assistant.jsonl`
(a prompt, a tool-use-only assistant record with `usage`, its `tool_result`,
and a text reply) cover the shapes B1 did not.

Tests live in `internal/archive/parser_v06_test.go`: counts and turn kinds per
harness, Claude and Codex result linkage with arguments, Codex event dedupe,
Cursor turns and outcome, positional linkage, refusal to mislink an unknown
call ID, hook evidence outranking a native turn end, filter-2 regeneration, and
the retained hidden-role rejection. Two existing assertions changed with the
new definitions: the Codex fixture's turn count is now a known `0` rather than
unknown, and `internal/cli/linked_review_test.go` checks for `"hook_finals"`
instead of `"turns":` to prove the metadata-only path prints no normalized view
(`counts.turns` now appears in metadata).

Review fixes (on the branch, before merge):

- **Streamed usage counted once per message.** Claude Code writes one JSONL
  record per content block of a single API message, and each record repeats
  the same `message.id` and `message.usage`; summing per record counted one
  response as many. Token accounting is now attributed to the `id` of the
  object that carries it (`message.id`), the latest record for an id replaces
  the earlier ones, and only accounting with no identity (Codex
  `turn_token_usage`) is summed as it comes.
- **A stripped injected block is not a prompt.** Filter 3 removes the text of
  a `<system-reminder>`/`<user_instructions>` block, but an array-shaped
  message keeps the bare `{type: "text"}` block, which the parser counted as
  non-tool-result content. That made a tool-result record with a reminder
  beside it, or a prompt that was only injected instructions, a human prompt.
  A text-carrying block (`text`, `input_text`, `output_text`, or untyped)
  with no retained text now counts as nothing.
- Fixture `claude-streamed-usage.jsonl` covers both shapes; tests also pin
  that a Codex injected-only `input_text` prompt is ignored and that a bundle
  holding native text leaves every structure-derived count, the token counts
  included, unknown.

Left open: `counts.messages` and per-model `turn_count` still count each
streamed Claude record, so an assistant message split into text and tool-use
records counts twice; the plan defines messages as records, so this is noted
rather than changed. Codex dedupe keys on `call_id`/`item.id` as planned; if a
real rollout's `item_completed` item carries an id unrelated to the call's
`call_id`, the same work is counted twice, which only a real transcript can
confirm.

## PR B3 — Reader performance

No schema, filter, parser, or adapter version changes: this only changes how
`list` and `show` reach the metadata they already read.

1. **Scoped, concurrent listing.** `reader.ListMetadataWithOptions` lists
   `sessions/<harness>/` when a harness filter is given instead of the whole
   archive, skips every listed key that is not a `metadata.json` sidecar
   before any download, and reads sidecars with at most 8 requests in flight.
   Work is dispatched in key order and stops after a failure, so the error
   reported is still the first failing sidecar in key order, exactly as the
   sequential read reported it. Results stay newest capture first (now a
   stable sort). `ListMetadata` keeps its signature.
2. **Direct lookup for `show`.** `FindMetadataKeys` reads the sidecar key
   under each harness this build publishes (`claude`, `codex`, `cursor`) and
   lists the archive only if none exists, so a session under an unknown
   harness is still found. Two hits stay ambiguous; a read error other than
   not-found is returned rather than treated as absence. `show --harness` was
   already one read and stays one read.
3. **Disposable metadata cache.** `list` keeps a copy of each sidecar under
   `AGENT_ARCHIVE_HOME/cache/metadata/`, one file per object key, stamped with
   the ETag the listing reported. The listing still runs every time; a
   sidecar whose ETag is unchanged is read from disk, a changed ETag is
   downloaded again, and entries under the listed prefix that the listing no
   longer returns are deleted, which keeps the cache inside retention. A
   harness-scoped listing evicts only within its own prefix. Directories are
   0700 and files 0600, written atomically (temp file, rename) by a writer
   local to the cache that does not fsync: the cache is rebuilt from the
   store on any miss, and each entry records a SHA-256 of its bytes so a torn
   or altered file is a miss rather than a wrong answer. The cache refuses
   any key that is not a metadata sidecar, so it never holds source bundles,
   and any cache failure is a miss, never a failed `list`. `list --no-cache`
   bypasses it. `show` does not use it. `uninstall --delete-local-data` now
   treats `cache` as agent-archive's own entry.

   Staleness: the bytes come from a Get that runs after the listing. S3, R2
   and MinIO report a single-part object's MD5 as its ETag (the collector
   publishes sidecars with one PutObject), so when the listed ETag is a bare
   MD5 the bytes are cached only if they hash to it; bytes rewritten between
   the listing and the download are not cached and are downloaded again next
   time. An ETag in another form (multipart `-N`, SSE-KMS) cannot be checked
   and is cached as listed; that entry could be wrong only if the object was
   rewritten after the listing and rewritten back to the listed bytes before
   the next listing. That residual window is accepted; `--no-cache` bypasses
   it.

`storage.MemoryStore` now reports an ETag on `List`: the bare MD5 of the
bytes, as S3, R2 and MinIO do for a single-part object, so the cache and its
ETag check are testable in memory. The fake S3 server in `storage_test.go`
returns a quoted ETag and the round-trip test asserts the store trims it.

Measured locally against the in-memory store with 300 sidecars: an uncached
list takes about 1 ms, a cold cache about 25 ms, and a warm cache about 4 ms.
(With `local.WriteBytes`, which fsyncs every file, the cold case was about
1.3 s.)

`readSidecars` stops dispatching when the caller's context is done and
reports the cancellation even when the store ignores the context; without
that a cancelled `list` could return a partial result with no error. It also
re-checks for a failure after acquiring a slot, so no read starts after one.

Tests: `internal/reader/performance_test.go` asserts the listed prefix for a
harness filter, that only sidecars are read, the concurrency bound, first-error
ordering, context cancellation (no read in flight after return, not every
sidecar read, cancellation reported whether or not the store observes the
context), direct lookup with listing fallback and ambiguity, cache hits, ETag
refresh, eviction (including scoped eviction), permissions, metadata-only
content, recovery from a damaged entry, refusal of bytes that do not hash to
the listed ETag, caching under an unverifiable ETag, and a miss for an entry
whose bytes were altered on disk.
`internal/cli/inspect_performance_test.go` asserts that `show --harness` is one
read and no listing, that `show` without a harness does not list, and that
`list`, a repeated `list`, `list --no-cache`, and `list --harness` read 1, 0,
1, and 0 sidecars. `internal/cli/uninstall_test.go` asserts that
`--delete-local-data` removes `cache/` and does not report it as a leftover.
`internal/storage/storage_test.go` asserts the memory store's MD5 ETag.

Local verification: `go build ./...`, `go vet ./...`, `go test -race ./...`
and `gofmt -l .` clean.
