# Local session discovery: source evidence and decision

This records the bounded source investigation behind the revised
[discovery proposal](local-session-discovery.md). It describes source-format
support and its limits; it does not establish released desktop acceptance or
announce feature availability.

## Capture decision

Capture consent applies to supported session records in approved local Codex
homes, included projects, and the selected destination. Native start time must
fall within the current authorization generation and an unpaused interval.
Excluded projects, invalid metadata, identity conflicts, removal records, and
identifiable unsupported imports or inherited histories remain ineligible.
All publication uses the existing privacy filter and read-back verification.

An indistinguishable recent copy is eligible under those same rules. Copying an
old session does not change its native start, and a copy of an already registered
session does not create a second archive identity. The decision accepts the
recent-copy limitation; it does not assume that metadata proves local execution.
The frequency of copying or synchronizing recent sessions has not been measured.

## Evidence boundary

The investigation uses public upstream source and disposable synthetic probes,
not user transcripts, settings, credentials, or storage. Source presence, file
appearance times, and reconstructed database entries establish availability on
this computer, not where the task executed. Native first-task metadata can
identify known importer shapes without establishing universal provenance.

## Pinned observations

The 2026-10-02 follow-up was bounded to 30 minutes of public source inspection
and existing disposable-probe evidence. It added no runtime probe. The inspected
release was [Codex `rust-v0.159.3`](https://github.com/openai/codex/tree/01fc69f4026735edfdf6789820549727a4867b11),
commit `01fc69f4026735edfdf6789820549727a4867b11`. The previously executed Linux
binary's SHA-256 was
`8bf204b36a2f6dd0dab73aa2f639892e67ef9ac8befccb4a05b1496ebf25c479`.

| Observation | Result and limit |
| --- | --- |
| Actual Linux TUI and exec tasks at 0.159.3 | `cli/codex-tui` and `exec/codex_exec` produced native first-task IDs and start metadata. These were CLI probes. |
| Synthetic stdio client named `Codex Desktop` | Produced `vscode/Codex Desktop` metadata. The harness selected that client name; this was not a desktop GUI probe. |
| Copy only a native rollout into a fresh Codex home, then initialize app-server without starting a task | Codex reconstructed matching SQLite metadata. The database entry does not prove originating execution. |
| Built-in external-agent importer | First imported task has `external-import-turn-N` and null root, even with fresh session metadata. Inspect the first task; a later native resume must not override it. |
| Native paginated history without inherited markers | Occurred in the existing native probes. Paginated alone is not an import/fork discriminator. |
| Diagnostic process logs and live `thread/started` events | Logs can be lost/pruned or arise from resume; events also cover forks/reviews and are not retrospectively available to a scheduled scanner. No reliable durable execution-host discriminator was established. |

The [protocol](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/protocol/src/protocol.rs),
[native producer](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/core/src/session/mod.rs),
and [imported task producer](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/external-agent-migration/src/sessions/export.rs)
provide source-specific classification evidence. Parent/fork/history-base,
non-user `thread_source`, and spawned-agent markers can supply conservative
exclusions. Absence of those markers supplies no proof of execution location.

The inspected successor [Codex `rust-v0.160.0`](https://github.com/openai/codex/tree/a956835d020762cb2b570053af06f643a11c0ecc)
peels to `a956835d020762cb2b570053af06f643a11c0ecc`. Exact-commit comparison found
its relevant protocol, rollout metadata/recorder, importer, initialization, and
state extraction unchanged. This is source compatibility for the inspected
fields; no 0.160.0 runtime or actual desktop was exercised.

## Initial format rules

Implementation should pin supported producer/version combinations explicitly,
with desktop source compatibility distinguished from GUI acceptance. Native
legacy/absent or paginated history without parent/fork/history-base markers can
qualify. Compressed or referenced histories require separate support.

Read the first task event within fixed metadata bounds. Reject known import
markers, malformed native IDs, unsupported source forms, and invalid start
metadata; never skip a bad first event in favor of a later native task. Header
creation time governs consent. A task may begin long after creation during an
idle TUI, so task delay is not an upper eligibility bound. Test timestamp
precision and future-skew handling explicitly.

These checks distinguish supported shapes and known inherited content. They do
not distinguish every recent copy. The bounded investigation does not claim to
have exhausted every possible Codex signal.

## Remaining release work

Source inspection and a mock client cannot substitute for an actual desktop
probe. Desktop onboarding with hooks absent or unapproved, disposable storage
publication/read-back, worktree and lifecycle cases, writer compatibility, and
release-machine performance remain acceptance gates in the proposal. Source
ambiguity alone is no longer a gate. Keep activation separate from independently
reviewable foundations, scanning, and onboarding changes.
