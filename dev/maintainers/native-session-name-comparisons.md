# Native session name comparisons

Status: **PARTIAL — sandbox execution recorded; native UI acceptance BLOCKED**.
The [2026-10-07 execution record](acceptance/native-session-name-comparison-2026-10-07.md)
distinguishes synthetic coverage from actual read-only identity observations and
the remaining environment blockers. This is a manual work package, not completed
UI acceptance. It follows [#343](https://github.com/wangjohn/agent-archive/issues/343)
and implementations [#346](https://github.com/wangjohn/agent-archive/pull/346),
[#350](https://github.com/wangjohn/agent-archive/pull/350), and
[#353](https://github.com/wangjohn/agent-archive/pull/353). The starting code
snapshot is `74941803781ccbb48bdcbbc1322ca0a2c58bc392`; record the actual
candidate commit when executing. Use the [evidence template](acceptance/native-session-name-comparison-template.md)
for each producer, storage form, and lookup mode.

## What a comparison proves

Match the native session UUID to `native_session_id` in the archive metadata,
then use its full `session_id` for archive commands. Titles that look alike,
short IDs, and native previews do not establish identity. Record machine and
home aliases too: two homes with the same UUID are separate observations.
For Codex, obtain the UUID from that thread's native details or verified
rollout `session_meta.id`; for Claude, use the picker session's verified
transcript/session identifier. If the UI-to-UUID connection cannot be proven,
leave the row blocked rather than select the nearest title.

Compare the Codex desktop sidebar or Claude Code CLI session picker with
`name` and the displayed heading of both `list` and `show`. `title` is a separate
first-human-prompt preview. Whitespace collapse, credential redaction, terminal
escaping and the 128-character limit are expected archive transformations;
record which applies. Use short synthetic, nonsecret names for exact equality.
An unnamed session can legitimately show the archive prompt fallback; this is
not proof of every native picker fallback. CLI, extension and desktop surfaces
are separate: a Claude CLI result cannot pass Claude desktop coverage.

## Environment and authorization before execution

Follow [CONTRIBUTING](../../CONTRIBUTING.md#never-test-against-your-real-machine)
and the [sandbox recipe](../contributing/testing.md#running-the-binary-by-hand-in-a-sandbox).
Use a disposable macOS account or VM for desktop comparisons, disposable native
homes, a synthetic project, and a disposable local S3-compatible destination.
A temporary `HOME` alone does not isolate Keychain or desktop settings. Keep
credentials in the disposable environment; use the documented launchctl stub
when testing by hand, and never the host's real login user manager on Linux.
Record versions with `agent-archive --version`, `codex --version`, and
`claude --version`, plus the desktop build separately. A CLI version does not
prove the producer of every retained rollout.

Prepare existing synthetic sessions and an already configured, working archive
in that disposable environment. If preparing these requires paid inference,
login, or app configuration changes, obtain the user's authorization for that
later run first. Do not initiate paid work merely to populate this checklist.
Use normal sandbox capture of supported sessions; do not invent registration,
ownership or source-history authority to make a row pass. A synthetic fixture
can check parser behavior but cannot pass the UI rows below.

Separately, the user may later authorize **read-only** observations of existing
real UI titles and already published archive metadata. Those observations must
not run setup, sync, backfill, change native settings/auth, start the native
transport, or rename real chats. Such snapshots cannot pass mutation or
coexistence rows. Do not copy private transcripts into this repository.
Keep raw screenshots, titles, UUID mappings, metadata and bucket keys locally
in a restricted evidence directory. Commit only a synthetic/redacted summary
with stable case aliases; public UUIDs and private titles are not required.

## Supported combinations and boundaries

Run each applicable row separately; unavailable combinations are coverage gaps,
not successes.

| Combination | Expected scope |
| --- | --- |
| Claude Code transcript-backed CLI picker | Latest custom title wins over generated `ai-title`, regardless of later generated records. Missing-title prompt fallback is separate. |
| Codex 0.159.2, `files`, settled default `state_5.sqlite`, ordinary retained source | Verified matching rollout path, producer and SQLite shape required. Paginated native history uses `threads.name`; legacy uses distinct native title or last usable index name. |
| Codex `files`, live WAL/SHM/journal or unsupported/relocated storage | Unavailable; preserve prior verified name. Do not remove WAL/SHM, checkpoint the user's DB, or copy only its main file to manufacture support. Production has no positive index-only historical-placement proof. |
| Codex 0.159.2, explicit `native` opt-in, ordinary retained source | Metadata-only app-server requests under production budgets; startup may write native storage. Real desktop coexistence, auth behavior and sidebar parity still require this comparison. |
| Codex archive history source set, even with an ordinary active source | External ordinary-name lookup is excluded. Verify existing history publication/readback remains intact, not universal name refresh. |

See [Codex session names](../../docs/reference/codex-session-names.md) for the
precise guards, budgets, fallback and config procedure. Change `codex_name_lookup`
only in the disposable archive config, with collection paused and a backup.
Native mode is opt-in; do not assume it repairs every unsupported history form
or reads all merged SQLite settings safely. Paginated **native** storage and
archive **history source sets** are different contracts.

Disposable metadata lookup timings reported during implementation (warm 598 ms
and a later cold 956 ms probe) are narrow process/lookup observations. Preserve
the exact probe artifacts and candidate if citing them; the reference doc's
earlier cold instrumentation failure is a separate attempt. None compared real
user titles with the sidebar, proved desktop coexistence or authentication, or
established a latency distribution. They cannot pass a row in this protocol.

## Commands and baseline

In the disposable environment, choose an evidence directory outside source
control. The examples assume a working sandbox archive; they do not configure
one. Replace `codex` with `claude` for Claude cases. Keep the native UUID mapping
in the local record and assign `archive_id` from the matching metadata row.

```sh
agent-archive --version
agent-archive status --verbose --no-pager
agent-archive list --harness codex --all-projects --limit 0 --json > "$evidence/before-list.json"
agent-archive list --harness codex --all-projects --limit 0 --verbose --no-pager > "$evidence/before-list.txt"
agent-archive show "$archive_id" --harness codex --json > "$evidence/before-show.json"
agent-archive show "$archive_id" --harness codex --no-pager > "$evidence/before-show.txt"
```

Confirm `native_session_id`, `session_id`, `machine_id`, `project_id`, `name`,
`title`, parser/adapter/filter versions, source schema and `source_bundle.sha256`.
Capture the native UI label and UUID proof at the same stage. Record conversation
activity fields (`started_at`, `ended_at`, `captured_at`, `counts`) and relative
text-list order. JSON ordering is newest capture first; do not compare it to
text activity ordering as if they were the same. `metadata_derived_at` and source
checksum can change during a rename publication.

`list --limit 0` returns all matching rows; the ordinary default limit is 50.
There is no CLI page-token flag. Exercise small limits and `--limit 0` to test
archive listing index coverage; use the native UI's own scrolling/loading for
sidebar or picker pagination. `list --json` includes subagents; ordinary text
lists hide them. Do not mistake that difference for lost naming.

For each publication step below, wait for the disposable collector or run
`agent-archive sync` **only inside that prepared sandbox**. Record the pass time,
status and read-back result. Allow documented budget/backoff deferral across
passes, recording every attempted pass and a declared observation deadline.
Timeout, storage/Keychain failure (including the earlier `OSStatus -50`), lack
of admission or ownership, or an unavailable provider is BLOCKED/PENDING, not
a title match. Re-run the commands with a new stage prefix after publication.

## Scenario sequence

| Case | Action in disposable environment | Required observation | Status |
| --- | --- | --- | --- |
| C1 / A1 identity and baseline | Open the existing Codex sidebar / Claude CLI picker entry; prove UUID and archived ownership. Include two sessions sharing a synthetic title. | Correct UUID-matched `name` and displayed heading in list/show; `title` remains the prompt preview. | BLOCKED; see execution record |
| C2 / A2 rename without prompt | Rename using that native app's supported UI (`/rename` for Claude); send no further prompt. Record transcript signature before/after. Publish and read back. | New name arrives without another conversation turn. Codex external-name refresh preserves transcript/activity; Claude may append a label record but activity/counts stay unchanged. | BLOCKED; see execution record |
| C3 / A3 delayed generated title | Observe a prepared session initially unnamed, then a native generated title arriving; record both times and records. No fabricated `ai-title` for UI acceptance. | Prompt fallback first, generated name after successful capture/publication, same UUID throughout. If paid work is necessary, defer until authorized. | BLOCKED; see execution record |
| A4 custom precedence after work | In an authorized synthetic Claude session, set custom name, do further work and observe a subsequent generated-title record. | Custom name still wins in picker/list/show. If producer emits no later generated record, precedence-after-generation remains PENDING; fixture coverage is separate. | BLOCKED; see execution record |
| C4 / A5 restart | Close and restart the disposable native app/CLI and archive reader; run a later collector pass. | UUID/selected name persists, no stale fallback; record both restarts and pass. | BLOCKED; see execution record |
| C5 / A6 unchanged pass | Make no native name/content change; run another collector pass and read archive. | No new name publication/source revision; activity, capture time, counts and order unchanged. Use publication evidence, not just equal displayed names. | BLOCKED; see execution record |
| C6 native desktop coexistence | Keep disposable desktop open while explicitly opted-in native lookup collects admitted ordinary sessions. | Archive-backed publication and UI remain responsive; no locks/migrations/auth surprises; process teardown verified. Record host effects and any refusal, not just RPC success. | BLOCKED; see execution record |
| C7 settled files / live WAL | Compare supported settled file case after normal app shutdown; separately observe refusal while WAL/SHM/journal is present. | Settled parity only for verified supported shape; unavailable case preserves prior name and history. No destructive DB manipulation. | BLOCKED; see execution record |
| C8 native pagination / archive limits | Observe an older native entry beyond initial visible rows; compare `list --limit 1`, default list, and `--limit 0`. | UUID maps after UI pagination; exact row appears in uncapped archive list/show; count/truncation behavior and top-level filtering remain correct. | BLOCKED; see execution record |
| C9 history compatibility | Use pre-existing synthetic legacy and paginated native cases, plus a separately captured archive history source set. | Ordinary supported names follow storage-specific rules; history source-set segments/revisions still read back unchanged, without external ordinary-name authority. Unknown producer/schema stays unavailable. | BLOCKED; see execution record |
| C10 / A7 archive-only read | Read published metadata on a disposable reader with no native agent/home present. | List/show use archived name; no native file reads or agent startup (observe with process/file tracing when available). | BLOCKED; see execution record |

Use separate template copies for file and native cases; a native fallback to files
must be recorded as fallback, not native success. Inspect only filtered retained
source evidence when verifying provenance; never publish raw app-server responses
or transcripts. Name-only changes must produce retained source evidence plus
metadata and listing revision through the normal writer. Equal strings alone do
not prove that publication or an unchanged no-op pass was correct.

## Closing a run

For every row record PASS, FAIL, BLOCKED or PENDING with local evidence references,
actual candidate and producer/build versions. PASS requires the observation,
UUID proof and archive readback; exclusions remain explicit. Summarize supported
combinations, failures and untested surfaces in a redacted record linked from
the acceptance sheet. Keep the work package pending until it has actual evidence;
unit tests and metadata lookup probes do not complete deferred native comparisons.
