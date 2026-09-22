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

## PR A3 — CLI correctness

No schema, filter, parser, or adapter version changes. `config.json` gains one
optional field, `installed_executable`.

1. **Uninstall purge finishes.** `uninstall --delete-local-data` now releases
   `hooks.lock`, `collector.lock` and `setup.lock` (each release idempotent,
   so a deferred second release cannot unlock a reused descriptor), removes
   the lock files, and removes the data directory when nothing unrelated is
   left in it; unrelated files still keep the directory and are named. A
   Keychain that is unavailable or refuses a delete no longer aborts after
   hooks and the LaunchAgent are gone: local files are removed anyway, and the
   uninstall reports as incomplete, naming the Keychain service and the
   account names it could not delete with the recovery for that failure.
   Those names are opaque Keychain references, not secrets; they are printed
   deliberately, because once `config.json` is gone nothing else records them.

   Review decision on the "never print credential references" rule: a
   reference is `setup-` plus 32 hex characters from `crypto/rand`
   (`local.ID`); it names a Keychain item and reveals nothing about the
   account, bucket, or secret, and after the purge it is the only handle the
   user has on the item. Uninstall therefore prints the service, the count,
   and one exact `security delete-generic-password -s agent-archive -a <ref>`
   command per item. The advice is uninstall-specific (unlock the Keychain,
   then run the command or use Keychain Access); it never points at `sync` or
   `setup`, which have nothing to act on after a purge. Every other command
   keeps the rule. Lock files are unlinked while still held and released
   afterwards, so a process that opens one during the purge gets a fresh
   inode of its own instead of acquiring an unlinked one after the release.
2. **Hooks check uses the installed path.** Setup records the executable it
   wrote into the hooks and LaunchAgent as `installed_executable`, in the same
   transaction. Status checks hooks against it, falling back to the running
   executable for configurations written before this field.
3. **Keychain failures are distinct.** A platform-independent
   `errorForOSStatus` maps `errSecItemNotFound` to `ErrKeychainItemNotFound`
   (still an `ErrMissingCredential`), `errSecInteractionNotAllowed` and
   `errSecAuthFailed` to `ErrKeychainLocked` (still an `ErrUnavailable`), and
   anything else to `KeychainStatusError` with its result code.
   `keychain_darwin.go` asserts at compile time that the Go-side codes equal
   the framework's. `kSecUseAuthenticationUIFail` is kept, so a background
   process never prompts. `sync` prints, and status's next action gives, one
   recovery per failure; status matches the text recorded in `status.json`.
4. **Only typed 404s are missing objects.** `isNotFound` accepts
   `types.NoSuchKey`, `types.NotFound`, or an HTTP response error with status
   404, and never matches error text. A 404 whose code is `NoSuchBucket` stays
   an error, since a missing bucket is misconfiguration, not an absent object.
5. **Trust is explained.** Status keeps `trust: "unknown"` and the human
   output now says trust is granted inside each app and is not observable from
   local files.

Tests: `internal/credentials/keychain_errors_test.go` (result-code mapping and
distinct, wrap-proof recovery text, with no real Keychain);
`internal/storage/not_found_test.go` (typed evidence only, and through the SDK
against a fake server: a missing key is `ErrNotFound`, a 403 saying "not
found" and a missing bucket are not); `internal/cli/cli_correctness_test.go`
(a purge leaves no files or directory; a refused or unavailable Keychain still
purges and names what it left; hooks read as installed when status runs from a
different path, with the fallback for older configurations; sync and status
give the locked, missing, and other recoveries and no Keychain advice for a
network failure; the trust explanation). `TestUninstallLeavesFilesItDidNotCreate`
now expects only the user's file to remain, since the lock files are removed.

Local verification: `go build ./...`, `go vet ./...`, `go test -race ./...`
and `gofmt -l .` clean.

## PR A2 — Collector and retention robustness

**Missing transcripts are a capture gap, not an error.** Claude Code deletes
transcripts after `cleanupPeriodDays` (30 by default) while retention keeps
sessions for 90, so a published session outliving its transcript is the steady
state. `processSession` now treats `os.ErrNotExist` like the oversize case:
`blockSession` with the new `BlockedReasonTranscriptMissing`, written once,
keeping the last published snapshot and acknowledging any request. `status`
lists it as a capture gap with its own explanation and no `Last error`, and
`sync` no longer exits 1 for it. It is the one recoverable block. When the file
returns, the cached state reverts to whatever status it had before the block
(recorded as `pre_block_status`), because a returning file identical to what
was captured produces no change for the normal comparison to clear it with. A
blocked parent is no longer re-notified about a published child on every pass
(each notification was written and then discarded by the block); the link is
announced once the parent recovers.

**Retention never expires unpublished work.** Whole-session expiry is skipped,
silently, while a request or a pending publication exists for the session, and
retried next pass. Once the work lands its capture time moves, so the session
then expires on its own schedule with its evidence archived. The pending check
is a stat (`LocalStore.HasPending`), never a decode of the pending source bytes.

**Retention sweeps every registration this machine owns.** `Sweep` no longer
takes `AcceptSession`. Two narrower predicates replace it. `CurrentDestination`
is false only for a session that predates `DestinationSince`; its local state is
pruned once it ages out, and not a single call reaches the current bucket for
it. `Publishable` is the collector's `AcceptSession`, and it decides only
whether outstanding work defers expiry. Without it, a session in an excluded
project whose Stop hook left a request would be deferred forever, since the
collector never processes it. An excluded project's published sessions now
leave the bucket on schedule.

**Unchanged sessions cost nothing.** Each pass fsynced the scan journal twice
per session and decoded the cached bundle several times, whether or not
anything changed. A completed scan that ends settled (published, declined, or
unchanged) now leaves a small `scan-signatures/<id>.json`. It records the
transcript's size and nanosecond mtime, taken before the read, plus the parser,
filter, and adapter versions. The next pass skips the session on one stat when
all of these hold: the signature matches, no request is pending, no
publication is pending, and no interrupted scan is journaled. In that case
nothing is opened, decoded, or written. A blocked session has no signature,
because `blockSession` removes it, so every gap is re-evaluated each pass.
Subagents are never skipped, because their publication also notifies a
parent. The signature is a separate file because reading the published cache,
a whole bundle, is exactly the cost being removed.

Measured with `TestUnchangedSessionsCostNoWritesAndStayFast` (Apple Silicon,
APFS, no race detector): 300 unchanged sessions of 400 KB take **25 ms per pass
(0.08 ms/session)**, against **12.2 s** for the same pass re-reading every
transcript. The test makes every published cache file unreadable during the
pass (any read would fail a session). It also compares size and mtime of every
file and directory under the home before and after, which catches a journal
entry created and removed within the pass. The same test then proves the
detector sees a full pass's writes. Only `status.json`, one write per pass, is
excluded. Under `-race` it checks the same invariants on 20 sessions, because
setup alone would take minutes.

The published cache now stores the bundle once. While a session is in its
normal published state, `last_published` carries only its time and
`same_as_bundle: true`. It gets its own copy again the moment a different
candidate (rate limited, declined, blocked) takes over the cached bundle, which
is the only case `nativeEvidenceExtends` needs both. Files written with two
copies still load.

Residual risks and limits:

- A transcript rewritten in place to exactly its previous byte length, whose
  mtime is also restored to the same nanosecond, is indistinguishable by stat
  and is skipped. No supported application does this, but it is not a proof.
  `TestUnchangedCheckSaysYesOnlyWhenNothingIsOwed` pins the case so it cannot
  widen unnoticed. A Cursor text transcript (no per-record timestamps, compared
  by prefix) is never skipped on a stat, and neither is any session with a
  pending request. A filesystem that stores only second-resolution mtimes
  weakens the guarantee to "same size, same second"; the nanosecond tests
  skip themselves there.
- A request that a hook writes mid-pass is seen on the next pass, as before.
- Blocked sessions stay on the full path by design. For a missing transcript
  that is one failed `open` per pass. A rewritten or oversize transcript is
  still re-filtered every pass, as before this PR.
- A missing transcript acknowledges its request, as the oversize case does.
  Hook-only evidence in that request is not lost, though: see the review
  notes below.
- A never-published registration's retention clock is the session start; see
  the review notes below.
- User-visible: setup's destination-change wording was updated in review to
  say that earlier sessions' local evidence is pruned after `RetentionDays`.

Tests: `internal/collector/robustness_test.go` (missing transcript blocks once,
keeps the snapshot, recovers on an identical and on a new file, and recovers
when it first appears; single-copy cache and the older two-copy shape; every
skip condition of the unchanged check, including a 1 ns mtime move and the
pinned residual case; same-length rewrite detection; the timed no-writes pass),
the blocked-parent notification test, `internal/retention/robustness_test.go`
(expiry waits for a pending publication and for a request, then proceeds;
unpublishable sessions expire despite outstanding work; every registration is
swept; previous-destination pruning with zero bucket calls), and
`internal/cli/retention_scope_test.go` (the same through `runOnePass`, plus
`status` reporting the gap without an error). Two existing tests used a missing
transcript to stand for a retryable error and now use a transcript with no
recognized records; the legacy-metadata test now removes the scan signature,
which real legacy state never has.

Review (Fable 5.1) fixes on the branch:

- **Hook evidence is held across a missing transcript.** `blockSession` for
  the recoverable reason stores the acknowledged request's evidence in the
  cache (`deferred_hook_evidence`), accumulating across requests while the
  file is away and never rewriting the cache when nothing arrived. When the
  transcript is readable again, `ClearRecoverableBlock` re-saves that evidence
  as an urgent request *before* rewriting the state, so the request machinery
  carries it durably (a crash at any point leaves it pending; a replay adds
  nothing the request already holds), and `processSession` reloads the request
  so the same pass publishes it. This works whether or not a capture preceded
  the block. Tests: `TestHookEvidenceHeldWhileTranscriptMissingPublishesOnReturn`
  and `TestHookEvidenceHeldBeforeFirstCapturePublishesWhenFileAppears`.
- **Never-published registrations expire.** Retention ages a session from its
  cached capture when it has one and from `SessionStartedAt` otherwise. Once
  expired, a session with no recorded publication and no pending publication
  has nothing in any bucket, so it is forgotten locally with zero remote calls
  (`PrunedSessions`); a pending publication may have reached storage, so that
  case keeps the remote path. Outstanding work still defers expiry, only when
  publishable. Tests: `TestNeverPublishedRegistrationExpiresLocallyWithoutTouchingTheBucket`
  and `TestNeverPublishedRegistrationWaitsForPublishableWork`.
- **A still-blocked session is never signed as settled.** The "unchanged"
  exit of `processSession` is reachable for a rewritten transcript that then
  sits untouched, and it wrote a scan signature, so a recorded gap was skipped
  on a stat from the second pass on, contrary to the plan's skip conditions.
  It now returns without a signature. Test:
  `TestUnchangedRewrittenTranscriptLeavesNoSignature`.
- The timed no-writes test asserts its one-second target only in the plain
  run; under `-race` it still proves nothing was read or written, without a
  wall-clock deadline that could flake on CI.
- Setup's destination-change message and `docs/install.md` now say old
  sessions stay published at the previous destination and their local
  evidence is removed from this Mac after the retention period.

Verified in review: retention reaches `deleteWholeSession` only through
`locallyExpired`, which is false while publishable work is outstanding, and
only after a metadata `Get` that returned success or `ErrNotFound`; any other
storage error aborts before any delete. `ForgetSession` is reached only after
that delete, or with zero bucket calls for a previous-destination or
never-published session. A parser, filter, or adapter version change misses
the scan signature and forces a full pass; the signature is written only at
the unchanged, declined, and published exits. `LoadLastPublished`, the
superseded-key reconstruction in `publishPending`, and `saveRepublishedMetadata`
all resolve the single-copy marker, and the two-copy on-disk shape still loads.

## PR C1 — Prompt and message accuracy

Filter version `4`, adapter version `0.4.0`, parser version `0.7.0`. Metadata
schema version 1 is unchanged; `counts.user_shell_commands` is a new optional
count. The Claude shapes below follow a structural probe of real transcripts
(tags and flags only); every fixture is synthetic.

1. **Codex startup shell.** An `item_completed` `CommandExecution` with
   `source: "unified_exec_startup"` is Codex starting the session's shell, and
   is no longer a tool call. A `CommandExecution` from any other source is
   still counted, and still deduplicated against its `response_item`.
2. **Streamed responses.** Claude Code writes one record per content block,
   all sharing `message.id`. `counts.messages` and each model's `turn_count`
   count distinct assistant message ids; a record without an id counts on its
   own. This closes the B2 review's open note, and that fixture's message
   count drops from 4 to 3. `NormalizedTurn` gains `message_id`.
3. **Harness records are not prompts.** Filter 4 retains `isMeta` and strips
   the text of every `isMeta` record (a `hidden_instruction_omitted` gap), so
   an expanded skill or command is no longer archived and no longer visible as
   a turn; its ids and parent link survive. The parser classifies user records
   by their leading tag: `<local-command-stdout|stderr|caveat>` and
   `<bash-stdout|stderr>` are `command_output`, `<bash-input>` is a
   `shell_command` counted in `counts.user_shell_commands`, and an `isMeta`
   record that still has content is `harness_meta`. A
   `<command-name>`/`<command-message>`/`<command-args>` slash command is a
   prompt only if an assistant record follows before the next prompt, slash
   command, or shell command (skipping harness records and tool results);
   otherwise, as for `/model` or `/clear`, it is a `local_command`.

Filter-3 bundles still regenerate. They lack the `isMeta` flag and keep the
expansion text, so a skill expansion stands in for its slash command: the
synthetic compatibility session gives 2 turns and 4 messages under parser 0.7
(parser 0.6 gave 8 and 11), and the same session captured under filter 4 gives
identical counts.

Fixtures: `codex-startup-shell.jsonl`, `claude-streamed-response.jsonl`
(three records of one `message.id`), `claude-local-command.jsonl` (`/model`
with caveat and stdout, no reply), `claude-skill-command.jsonl` (slash command,
`isMeta` expansion, reply), `claude-shell-command.jsonl` (`!` command and its
output). Tests: `internal/archive/filter_v4_test.go` (the flag and ids are
kept, only text is stripped, non-meta records are untouched) and
`internal/archive/parser_v07_test.go` (each item, slash-command resolution
edge cases, records without ids, and the filter-3 regeneration test).
`TestFilterVersionIsDeclaredInCaptureProvenance` now checks the build's own
versions instead of filter 3's literals.

Review fixes (on the branch, before merge): the `isMeta` allowlist entry
admits only a boolean, so a string or object under that key stays omitted
wherever the ordinary allowlist applies (tool-argument subtrees were already
retained whole under filter 3 and are unchanged); an `isMeta` record's text is
stripped at every depth, so a tool result or unknown block inside it keeps its
identifiers but loses its own `text` and nested content text; and the Codex
startup exclusion matches only a `CommandExecution` whose `source` is exactly
`unified_exec_startup`, never another completed item type or a
case-variant source. Tests: `TestFilterV4RetainsIsMetaOnlyAsABoolean`,
`TestFilterV4StripsNestedTextInMetaRecords`,
`TestParserV07StartupShellExclusionIsExact`,
`TestParserV07HarnessTagsAreRecognizedByPrefixOnly` (leading whitespace,
text blocks, and prompts that merely mention a tag), and
`TestParserV07FixturesScanDeterministically` (three scans of each new fixture
give byte-identical compressed bundles and metadata).

Left open: tag recognition is by prefix of the record's text, so a person
whose prompt literally begins with one of these tags would not be counted; the
tags are harness-reserved and the probe found them only in harness records.
Codex and Cursor report `user_shell_commands: 0`, since neither writes these
shapes. A Claude Code compaction summary (`isCompactSummary: true`, no
`isMeta`) is still a user record with text and so still counts as a prompt;
retaining that flag would be a further filter change and is not part of C1.

## PR C3 — Cursor first-prompt registration

No schema, filter, parser, or adapter version changes. Based on a live check of
the Cursor desktop app 3.21.13 (Cursor's Hooks output): a new Agent chat fires
no `sessionStart`; its first hook is `beforeSubmitPrompt` with
`transcript_path` null, then `afterAgentResponse` and `stop` name
`~/.cursor/projects/<workspace>/agent-transcripts/<id>/<id>.jsonl`. A resumed
chat's first prompt already names its non-empty transcript, and `sessionEnd`
can fire mid-turn. The PR A1 `sessionStart` rule therefore never registered a
Cursor app chat.

1. **Registration at the first prompt.** A never-seen Cursor conversation is
   registered at `beforeSubmitPrompt`, and still at `sessionStart`, when
   `transcript_path` is null, absent, or names an absolute missing or empty
   file. A non-empty transcript is a resume, declined with
   `session_start_unknown` for included projects only; a directory outside
   every project records nothing. `session_started_at` is the first hook's
   time, and the lifecycle evidence names the real event
   (`beforesubmitprompt`), never a fabricated start. A setup-in-progress
   diagnostic is recorded for a Cursor first prompt as for a start.
2. **Transcript path from later events.** A registration with no path takes
   it from a later `beforeSubmitPrompt`, `afterAgentResponse`, or `stop`, only
   when the path is absolute and named `<conversation_id>.jsonl`; a path, once
   set, is never replaced. The same name rule applies to a path offered at
   registration, so a mismatched path can never be recorded first and then
   become unreplaceable.
3. **Waiting, not failed.** The collector skips a registration with no path
   without error: no `LastError`, not counted as failed, any queued request
   kept, retried every pass, and published on the first pass after the path
   arrives.
4. Codex and Claude Code are unchanged. `docs/capture-capabilities.md`,
   `docs/agent-archive-session-eligibility.md`, and the Cursor capability
   text in `status` describe the observed 3.21.13 behavior.

Tests: `internal/cli/cursor_first_prompt_test.go` (a new chat registers at a
null-path first prompt, a sync while waiting records no error, the response
and stop supply the path, and the next sync publishes; a resumed chat is
declined with the diagnostic; wrong-name, relative, and non-`.jsonl` paths are
ignored and a set path is never replaced; Codex and Claude prompts still
register nothing) and `internal/collector/waiting_transcript_test.go`. Three
existing tests that asserted "a Cursor start with no path proves nothing" now
give their resume a non-empty transcript, or expect the new registration.

Review (C3): the fresh-start proof also accepts a path to a file that exists
but holds no bytes, and a sync in that state failed every pass ("filtered
transcript has no retained evidence", `LastError` set) — reachable through a
`sessionStart` that names the file, and through a later event that names it
before Cursor flushes the first record. `processSession` now treats an empty
transcript with no publication yet as the same waiting state: no error, the
request kept, re-read next pass. A file emptied after a publication is still
a rewrite. Tests added: `TestRegistrationWithEmptyTranscriptIsWaitingNotFailed`
(collector); overlapping first-prompt, response and stop hooks register once
under `hooks.lock`; a waiting chat shows in `status` as
"hook observed; waiting for capture" with no error; a later prompt (and a
`sessionStart`) on a registered chat keeps `session_started_at`; a chat in an
excluded project nested inside an included one records nothing at any event
(A1 nearest-ancestor resolution of `workspace_roots`). Docs now list what
3.21.13 did not show.

Left open: retention treats any queued request, including deferred hook
evidence, as unfinished work. A Cursor chat with transcripts disabled
registers, never receives a path, and keeps a request, so its registration
and hook evidence (including any final-response text) stay local instead of
expiring with retention. Suggested fix, in retention: a request for a
registration with no transcript path does not block expiry. The same chat
counts as pending in `status`, and `setup` refuses a storage-destination
change while any session is pending, so such a chat also blocks that change
until it is cleared. Also fixed in review: during a `setup` transaction, a
Cursor `beforeSubmitPrompt` for an already-registered chat recorded the
setup-in-progress diagnostic as if it were a start; `recordSetupInProgress`
now checks, lock-free, whether the chat is registered and explains only a
never-seen chat's first prompt.

Local verification: `go build ./...`, `go vet ./...`, `go test -race ./...`
and `gofmt -l .` clean.

## PR C2 — Status and race fixes

**Moved or deleted binary.** Every installed hook and the LaunchAgent run the
executable setup recorded in `installed_executable`. When that file was moved
or deleted, the hook configuration still matched exactly, so `status` called
the hooks installed. launchd also reports a job whose program is gone as
loaded, so the background looked healthy while every hook and every scheduled
collection failed. `status` now checks that `installed_executable` exists, is
a regular file, and is executable. If it is not:

- every app's hooks are `broken` (JSON `hooks: "broken"`);
- a warning names the path and why;
- the next action is to rerun `agent-archive setup` from the binary's new
  location.

It also reads the program the LaunchAgent plist actually runs, using
`hooks.LaunchAgentProgram`, which sits next to the plist writer and
round-trips XML-escaped paths. When that program is gone, the background is
reported as `broken`. This also catches a LaunchAgent left pointing at an
older install while the hooks are fine.

The moved binary outranks every other next action except an uninstalled
archive or a pending setup recovery. That includes pause, because pausing
does not stop the apps from running hooks that now fail. An uninstalled
archive reports nothing, since deleting the binary is the expected last step.
Test environments now install a real stand-in executable instead of a
fictional `/opt` path.

**Retention and hook requests.** Retention decided a session had expired
from a snapshot of pending requests taken at the start of the sweep. A hook
request written between that snapshot and `ForgetSession` was deleted with
the session. All three forget paths (whole-session expiry, never-published
pruning, previous-destination pruning) now go through
`LocalStore.ForgetIdleSession`. It takes the per-session request lock hooks
hold while writing, rechecks for a request or pending publication under it,
and keeps a session that gained work. A session the collector no longer
publishes is forgotten regardless, as before.

The remote deletion stays outside the lock, because a hook waits for that
lock on the user's turn. If a request arrives during it, the objects are
already gone, but the next collector pass republishes the whole session with
that evidence (every publication is complete, not a delta). If the request
added nothing, the pass acknowledges it and the next sweep finishes the
expiry.

The other side of the protocol: `saveRequest` refuses, under the lock, to
write for a session whose registration is gone (`ErrSessionNotRegistered`).
Otherwise a hook that looked the registration up just before it was forgotten
would leave an orphan request that nothing reads. `ForgetSession` removes the
registration before it unlinks the lock file. The lock is a `flock` on that
file's inode, so a hook that locks the fresh file afterwards always finds the
registration gone.

**Diagnostics.** Every capture-diagnostic write and setup's post-commit prune
now take `diagnostics.lock`. The hook side waits at most 50 ms and drops the
diagnostic on timeout; the prune waits up to 2 s. A lock alone would not have
been enough. The hook decided the project was included from a configuration
snapshot that can predate setup's commit, so under the lock it rereads the
committed configuration and drops a diagnostic for a project that is no
longer included. Setup prunes after it commits, so either order is safe: a
hook that writes first is pruned, and a hook that writes second sees the
exclusion. The hook path gains only that bounded wait plus one small config
read, and only when a diagnostic is recorded.

**Uninstall.** PR A2's `scan-signatures/` directory, which every collector
pass creates, was missing from uninstall's owned entries. On `main` a
`--delete-local-data` purge after even one sync exited 1 with "Uninstall
incomplete: unrelated files were kept … scan-signatures". It and
`diagnostics.lock` are now owned entries.

Tests:

- `internal/cli/moved_binary_test.go`: deleted, moved, and non-executable
  binaries through `readStatus`, `status --json`, and text output; the moved
  binary outranking pause; nothing reported after uninstall; a background-only
  break naming the LaunchAgent's own path.
- `internal/hooks`: the plist reader round-trips `LaunchAgent` output and
  rejects plists with no program.
- `internal/collector/forget_test.go`: the locked recheck for a request, a
  pending publication, and an unpublishable session; no orphan request after
  a forget.
- `internal/retention/request_race_test.go`: a hook request injected between
  the snapshot and the forget, by running the hook inside the first remote
  delete, is kept and republished with its evidence. A 30-round concurrent
  race of a hook against a sweep checks the invariant: a request
  `SaveRequest` reported as written is never lost, and a refused one leaves
  nothing behind.
- `internal/cli/diagnostics_lock_test.go`: the stale-snapshot resurrection
  case, deterministically; the bounded wait and drop; the prune waiting for a
  holder; a 40-round concurrent commit-and-prune race against two hooks.
  Removing the recheck fails it on resurrection, and removing the lock fails
  it on lost updates.
- A purge test that runs collector and diagnostic state, which fails without
  the uninstall fix.

Limits:

- **The 50 ms drop happens on a real machine.** Under fsync load a hook can
  find the lock held past 50 ms and drop its diagnostic. The race test raises
  that wait for itself, so it measures lost updates alone.
- **Race tests are probabilistic.** How rounds split between interleavings
  varies by machine and race-detector mode; the deterministic tests cover
  each interleaving that matters.
- **Only the plist's program path is checked.** `status` does not check
  whether launchd has the current plist loaded.

Review fixes on the same branch:

- **Subagent candidate whose parent was forgotten.** `rejectSubagentCandidate`
  notifies the parent through `SaveRequest`; with the new orphan guard that
  write is refused for a parent retention has forgotten, and the candidate
  was then never acknowledged, so the collector reported the same permanent
  condition on every pass. The refusal is now treated as "nobody to notify"
  and the candidate is acknowledged after one report.
- **Hook exit.** A hook whose request write is refused because retention
  forgot the session between its lookup and its write already exited 0; it
  now also says nothing, since that is the race's intended outcome.
- **Transcript-less registrations and retention.** Cursor (PR C3) registers a
  chat at its first prompt, before a transcript path exists, and the path
  normally arrives with a later hook; with the app's transcripts disabled it
  never does. A queued request for such a registration (it can carry
  last-message text) counted as unfinished work, so the session never
  expired. Once the session itself is older than the retention window, a
  request no longer defers expiry for a registration with no transcript
  path; it is forgotten locally under the same request lock, with zero
  bucket calls, since it can never have published. Registrations without a
  path can exist on `main` already (`hook.go` allows an empty path).
- The purge test also exercises the lineage ledger and the reader cache;
  `docs/install.md` names the `broken` hook and background states.

## PR C4 — Compaction summaries

Filter 5, adapter 0.5.0, parser 0.8.0.

After `/compact` or auto-compaction, Claude Code writes a
`{"type":"system","subtype":"compact_boundary"}` record and then a user
record marked `isCompactSummary: true` (usually also
`isVisibleInTranscriptOnly: true`) whose text is a model-written summary of
the earlier conversation. Filter 4 dropped the boundary as an unknown record
type and the flags as unknown keys, so parser 0.7 counted each summary as a
human prompt and a message.

**Filter 5.** Keeps exactly the `compact_boundary` record: `type`, `subtype`,
its ids, a parseable timestamp, and `isSidechain`. It is rebuilt from typed
values, and nothing else of the record is kept (see
`docs/agent-archive-privacy.md`). The flag `isSidechain` goes slightly beyond
the plan's list, for one reason: without it, a subagent's compaction inlined
in its parent transcript would count toward the parent's compactions. Every
other system record is still hidden, and only the Claude adapter admits the
boundary. `isCompactSummary` and `isVisibleInTranscriptOnly` are retained as
booleans only, under the same rule the #8 review gave `isMeta`. The summary's
own text is kept.

**Parser 0.8.**
- A user record with `isCompactSummary: true` is turn kind `compact_summary`.
  `show --normalized` prints it with its text, and it counts as neither a
  prompt, a message, nor a model turn.
- The summary ends the scan that decides whether a slash command was
  answered, so `/compact` is never promoted to a prompt, even when an
  auto-continued assistant record follows.
- `counts.compactions` counts boundary records, or summaries when no boundary
  was retained, so each compaction counts once.
- The count is reported only for Claude Code bundles from filter 5 on. For
  filters 2–4, which could not observe compaction, and for other harnesses,
  it is absent (unknown), not zero.
- Filter 2/3/4 bundles still parse with parser 0.7's counts: nothing in them
  marks the summary, so it still reads as a prompt.

`schemas/metadata.schema.json` gains `counts.compactions`.

**Rewrite protection.** Compaction appends the boundary and summary to the
same JSONL file, so filtered records extend the published ones and
`nativeEvidenceExtends` lets the session publish normally. A collector test
proves this with the fixture. A second test pins the fallback: if a
compaction ever rewrote the earlier records instead, the session would be
recorded as a `transcript_rewritten` gap rather than overwritten. A filter or
adapter version change is already not a rewrite, so sessions captured under
filter 4 republish under filter 5.

Tests:
- `internal/archive/compaction_filter_test.go`:
  - the boundary keeps exactly its identity keys, and none of its text or
    metadata leaks;
  - the summary keeps its text and flags;
  - non-boolean flags are omitted and reported;
  - an ordinary system record stays hidden;
  - a malformed boundary keeps only its type and subtype;
  - Codex does not admit the boundary.
- `internal/archive/compaction_parser_test.go`:
  - fixture kinds and counts: two prompts, five messages, two compactions,
    three model turns;
  - filter 2/3/4 bundles with unchanged counts and unknown compactions;
  - Codex reports none, and an uncompacted filter-5 session reports zero;
  - one count per compaction, the summary fallback, and the sidechain
    exclusion;
  - `/compact` is not promoted across the summary.
- `internal/collector/compaction_test.go`: the appended compaction publishes,
  and a rewriting one is recorded as a gap.

Not verified: no transcript on the development machine contains a compaction,
so both the record shapes and the claim that compaction appends rather than
rewrites come from Claude Code's behavior, not from an observed file. The
fixture is synthetic. The first real compacted session should be checked
with `show --normalized` for the `compact_summary` kind and a nonzero
`counts.compactions`.

## PR C5 — Registration races

**Resume during expiry.** `handleSessionStart`'s continuation path loaded a
registration and saved it back without the per-session request lock that
retention's `ForgetIdleSession` holds. A session resumed at the instant
retention forgot it could be written back after retention removed both the
registration and its native-session index entry. The session's next start
then found no index entry and got a second archive ID, and the first
registration was left unreachable. Cursor's transcript-path adoption had the
same load-then-save shape. Two collector helpers now take the lock around the
whole read-check-write:

- **`UpdateRegistration`** loads the registration under the lock. When it is
  gone it reports `found=false` without calling the update, and the hook
  treats the session as never seen: the ordinary fresh-start rules decide. A
  resume is declined with its `session_start_unknown` diagnostic; a provable
  fresh start (a Claude Code `/clear`) registers again under a fresh ID. An
  error from the update saves nothing.
- **`RegisterNewSession`** saves a new registration under its archive ID's
  lock, after rechecking that the native-session index still points at that
  ID. If the entry changed or disappeared, it assigns a fresh ID. This closes
  the same window for a fresh start that found an index entry retention was
  about to remove.

`ForgetSession` now unlinks the request lock file last, after the
native-session index. The lock is a `flock` on that file's inode, so
unlinking it lets a waiting hook lock a fresh file at once. Everything a hook
rechecks under the lock must therefore already be gone; before this change
the index outlived the lock file.

**Waiting Cursor chats at a destination change.** A Cursor chat whose
transcript never arrives (transcripts turned off) registers at its first
prompt and keeps a queued request, but it can publish nothing until a path
arrives. Setup counted that request as pending work and refused to change the
storage destination, leaving the user stuck on the current bucket.
`pendingSessionCounts` now separates registrations with no transcript path,
no publication, and nothing in flight:

- The destination guard in `reviewChanges` and `applySetup` counts only the
  other pending sessions.
- `status` and uninstall still count the waiting chat as pending.
- After the change it falls behind the new `DestinationSince` like any
  unpublished session: the collector no longer accepts it, and retention
  prunes it locally.
- Setup's destination-change message says so when there are any.

Tests:

- `internal/cli/registration_race_test.go`:
  - a resume interleaved deterministically with a forget is declined and
    explained;
  - a `/clear` in the same interleaving registers under a fresh ID;
  - a 30-round concurrent start-versus-forget race checks that every
    registration stays reachable from its index.

  All three fail against `main`'s hook and collector.
- `internal/collector/register_race_test.go`:
  - `UpdateRegistration` reports a forgotten session and saves nothing on
    error;
  - `RegisterNewSession` does not reuse an index entry being forgotten.
- `internal/cli/destination_waiting_test.go`:
  - a waiting Cursor chat does not block a storage change, is explained, and
    falls behind the boundary (this fails against `main` with "1 session(s)
    still pending");
  - a chat with a transcript that has not published still blocks.

Limits:

- **Which ordering the race tests exercise depends on timing.** The
  interleaving tests hold the lock themselves and give the hook 100 ms to
  reach it. If the hook arrives later, the test still passes but exercises a
  different ordering; the invariant it checks holds in every ordering, and
  no assertion depends on elapsed time.
- **Stale lock files.** A hook that loses the race creates a fresh
  `request-locks/<id>.lock` for the forgotten ID (the lock helper opens with
  `O_CREATE`), and nothing removes it, since the ID is never registered
  again. It is an empty file; `saveRequest` left the same remnant before
  this PR. A hook can also end up holding a `flock` on the unlinked inode
  while another holds the fresh file, but only for an ID whose every record
  was removed before the unlink, and every holder rechecks those records
  under the lock, so neither can write anything for that ID.
- **Dropped lifecycle evidence.** A hook's lifecycle evidence write after the
  registration step takes the lock separately. If retention forgets the
  session in between, that evidence is dropped as `ErrSessionNotRegistered`,
  as it already was.

## PR H1 — Handoff prerequisites: notifications, plugin catalog, Codex output

Filter version `6`, adapter version `0.6.0`, parser version `0.9.0` (PR C4 took filter 5 / parser 0.8.0 first). Metadata
schema version 1 is unchanged. Found by probing real Claude Code, Codex, and
Cursor transcripts for `docs/agent-archive-handoff-spec.md`; every fixture is
synthetic.

1. **Task notifications are not prompts.** Claude Code writes a background
   task's completion as a user record with `origin.kind: "task-notification"`
   and `promptSource: "system"`; typed prompts carry `origin.kind: "human"`.
   On the probed review session 12 of 20 counted prompts were notifications.
   Filter 6 retains `origin` as `{kind}` only and `promptSource` as a string;
   the parser classifies a user record whose `origin.kind` is present and not
   `human` as `harness_notification`, which is not counted as a turn and does
   not end a slash command's scan for its reply. A record with no `origin`
   (every filter-5 or older bundle) is classified as before.
2. **Codex plugin catalog is not a prompt.** `<recommended_plugins>` joins the
   injected-instruction tags, so the catalog Codex prepends to the first user
   message is stripped and that message no longer counts as a prompt. An
   audit of every leading tag in the Codex user messages on the probe machine
   found only this tag and `<environment_context>`.
3. **Codex list-shaped tool output.** Current Codex writes
   `function_call_output.output` and `custom_tool_call_output.output` as a
   list of `{type: input_text, text}` blocks. `toolResultOutput` handled only
   strings, so every such result reported `output_bytes: 0`; it now joins the
   blocks' text the way message content is joined.

Not fixable from the record: the desktop app's "The app was quit while you
were working…" message has `promptSource: "sdk"` and no `origin`, exactly like
an SDK-submitted prompt, so it still counts as one (see
`docs/capture-capabilities.md`).

Fixtures: `claude-task-notification.jsonl` (a prompt, a reply, a notification
whose `origin` carries an extra member, a reply), `codex-list-output.jsonl`
(plugin catalog, environment context, a prompt, a function call and a custom
tool call each with list output, a reply). Tests:
`internal/archive/parser_v08_test.go`.

