# Agent Archive next release review

Review date: 2026-10-01. Follow-up: the export and clipboard defects are fixed with permanent regression coverage, and the changelog is corrected. With no existing CLI users, legacy v0.1.1 collectors are outside the launch compatibility contract. Final candidate CI and acceptance remain release gates; adding two required CI checks is pending GitHub owner reauthentication.

## Release scope

The last published release is [v0.1.1](https://github.com/wangjohn/agent-archive/releases/tag/v0.1.1), published on 2026-09-28. There is no published v0.2.0 release or fetched v0.2.0 tag. The reviewed candidate is `98f8bd0898abc8f22e4d67d567cf11ae2b88ee2f`: 233 commits, 1,010 changed files, and 215 changed production Go files since v0.1.1. During review, additional commits added pager coverage, reorganized and refined design records, recorded Linux amd64 acceptance, tightened Cursor cache permissions, and set systemd’s working directory to `/`. Those changes were inspected and included; none resolves the findings below.

This is a substantial feature release, rather than a small patch. It adds Linux support, systemd scheduling and file credentials; terminal and worktree handoff; installed agent skills; richer metadata and statistics; interactive browsing and project scope; guided storage setup; and archive listing optimizations. Review focused on the release diff and interactions between these features, with deeper inspection of paths that affect privacy, deletion, upgrades, credentials, setup recovery, and user-visible output. This is not a claim of exhaustive line-by-line inspection of every fixture and generated artifact.

The initial review authored no production edits. At the user’s request, follow-up fixes were applied after fast-forwarding to `fc868ae`, including the subsequent hook admission and guided R2 documentation changes. No tag or release was created.

## Review findings and follow-up dispositions

### Launch compatibility decision: legacy collectors require a full scan

Location: [`internal/reader/list_index.go`](../../internal/reader/list_index.go), `ListRecent`, particularly the ready-marker check and index-only iteration at lines 32–44. The CLI selects this path for limited JSON listings without project or query filters in [`internal/cli/inspect.go`](../../internal/cli/inspect.go).

After rebuilding the index, another machine running v0.1.1 can publish a brand-new session without an index hint. The ready marker remains valid, and the new reader never discovers that session. Hash verification catches an older writer replacing an already indexed sidecar, but cannot detect an entirely new unindexed key. The reader can therefore return the wrong newest sessions and claim `Complete: true` and an exact count.

Reproduced with the existing in-memory counting store: publish session 1, call `RebuildIndex`, publish newer session 2 without a hint, then compare `ListRecent` with limits 0 and 50. The full scan returns both sessions with session 2 first. The indexed call returns only session 1, with `Complete: true` and `TotalMatched: 1`. The added temporary regression test fails on the candidate. No real archive was accessed.

**Disposition:** the user confirmed there are no existing CLI users. A rolling-upgrade protocol is therefore not required for this launch. Retain the fast indexed path and explicitly document that all writers must publish hints before metadata. Current collectors meet that contract. Development buckets with older metadata-only collectors must use a full scan until those collectors stop, then rebuild the index. The reproduction remains evidence of the compatibility boundary, not a supported-client release blocker.

**Temporary user workaround:** `list --json --all-projects --limit 0` uses the authoritative scan. Ordinary text listings also use it. This issue affects reading, not whether the transcript was uploaded.

### Resolved: stats export can replace a file without force

Location: [`internal/cli/stats_html.go`](../../internal/cli/stats_html.go), `writeStatsHTMLFile`, lines 192–198.

The atomic create path uses `os.Link`, but any error other than `os.ErrExist` falls back to `os.Rename`. On a filesystem where hard links are unsupported, a report created after the preceding `Lstat` can be replaced by that rename even though the user did not pass `--force`. This violates the command's explicit refusal to overwrite existing reports and can lose a user's file.

The temporary fault-injection test replaces only the link operation: it creates an existing destination after the last existence check and returns an unsupported-operation error. The test then checks that a no-force export preserves the destination. This exercises the fallback without requiring a special filesystem; its result is recorded below. The injection does not modify repository source.

**Implemented:** the no-force path now returns link failures without a replacing rename. Permanent fault-injection coverage preserves concurrent destination content for collisions and unsupported links.

**Original recommendation:** retain an atomic create-only guarantee for the no-force path. A safe initial fix is to report the link failure instead of falling back to a replacing rename. If supporting filesystems without hard links is required, use a create-only operation and define cleanup on partial writes. Test concurrent destination creation, unsupported links, and preservation of existing content separately from the force path.

### Resolved: Linux handoff offers a clipboard action that requires macOS

Location: [`internal/cli/handoff_destination.go`](../../internal/cli/handoff_destination.go), the unconditional copy menu entry at line 50 and `Env.clipboard` at lines 267–277.

The Linux release presents “copy to the clipboard,” but the production implementation always starts `pbcopy`. A normal Linux installation cannot execute it. Existing tests inject a clipboard callback, so they verify the menu without exercising platform selection.

**Implemented:** platform and display-aware provider selection, explicit arguments and stdin, bounded execution, daemonized-provider handling, and omission of copy when unavailable. Synthetic executable tests exercise the production path without touching the real clipboard. The handoff guide is updated.

**Original recommendation:** detect a supported clipboard provider, such as `wl-copy` for Wayland or `xclip`/`xsel` for X11, and invoke it with explicit arguments and stdin. If none is available, omit or clearly disable the action and explain how to write a file instead. Preserve `pbcopy` on macOS. Cover provider selection and absent-provider behavior, and include a Linux desktop/headless handoff check. Update the handoff guide, which currently describes only `pbcopy`.

### Resolved: changelog uses an unpublished release as its baseline

Location: [`CHANGELOG.md`](../../CHANGELOG.md), the v0.2.0 heading at line 813 and comparison links at lines 1007–1008.

The file records v0.2.0 as released on 2026-09-29 and compares Unreleased against `v0.2.0`, although the latest published release and fetched release tag are v0.1.1. The broken comparison also hides which changes are actually pending. The roughly 800-line Unreleased section is useful development history, but is difficult for an upgrading user to navigate.

**Implemented:** one Unreleased section marked planned v0.2.0, corrected v0.1.1 comparison, a concise summary and script notes, and collapsible development history. Published installation pins remain at the actual release.

**Original recommendation:** prepare one v0.2.0 release section covering all changes since v0.1.1; assign the real release date when tagging and correct the comparison links. Lead with a short feature summary and migration notes. Keep the detailed history below it if desired. Update pinned README installation commands to the new tag only as part of the actual release preparation. Do not advertise the machine-pairing proposal as shipped functionality.

## Release gate adjustment

The live GitHub branch response on 2026-10-01 confirms `main` is protected. Its required contexts are `test (ubuntu-latest)`, `test (macos-14)`, `lint`, `fuzz`, and `verify`. It does not require the new `cross-build` or `real-systemd` jobs in [the Test workflow](../../.github/workflows/test.yml). The rulesets collection returned no additional rulesets. The systemd job explicitly documents that its stable name is used by branch protection.

Before the Linux release, an owner should add those two exact check names to the required set and confirm successful runs on the final commit. Source changes can otherwise merge with broken secondary architectures or a failing actual systemd integration job. This is a live configuration gap, separate from a code defect. Follow-up selected both additional checks while preserving all five existing checks and the up-to-date requirement. Saving reached GitHub’s Confirm access screen; owner reauthentication is required. The change is not yet saved or verified. Finish this settings step before release.

## Product and upgrade recommendations

The release notes should explain the new default project scope and the `--all-projects` escape hatch, the difference between launching in the current terminal and a new window/worktree, and how installed `/handoff` skills are refreshed. With no existing CLI users, focus acceptance on clean setup, credential handling on Linux, and successful capture from each enabled application. A customer migration walkthrough is unnecessary for this launch.

Scripts written against development or earlier builds need an explicit compatibility note: `list --json` changes from schema 1 in v0.1.1 to schema 4, removes the old `unavailable` field, defaults to 50 results, and may narrow to the current project. Consumers that need the former whole-archive behavior should use `--all-projects --limit 0`; consumers of bounded results must check `total_matched_known` before using `total_matched` as an exact count. These are intentional changes, but deserve a concise before-and-after example.

For shared buckets, every collector must use the current index publication protocol before an index is enabled. Metadata compatibility alone does not establish this requirement. Legacy metadata-only development writers remain outside the launch contract.

Statistics should continue to distinguish estimated cost from billed cost and preserve the default anonymization of exported project, skill, and tool names. Keep the guided R2 creation flow behind `AGENT_ARCHIVE_EXPERIMENTAL_R2_CREATE=1` until its [live acceptance checklist](../contributing/testing.md#live-acceptance-guided-r2-creation) is completed. The gate is present in the candidate; this review does not treat the experimental flow as generally available.

The pairing proposal is clearly marked unimplemented and is appropriate to keep as a design record. It should remain outside the release feature list.

The main testing improvement is coverage at system boundaries. The suite has extensive component, failure-recovery, and screen coverage, but injected clipboard callbacks bypass the real platform resolver, and indexed-reader tests assume writers participate in the new protocol. Permanent tests now exercise production clipboard selection and filesystem failures after an existence check. Old metadata-only writers are documented as unsupported with indexed listings. Those tests protect the user promises that the reproduced bugs violate.

## Areas reviewed without an additional confirmed blocker

| Area | Review focus |
| --- | --- |
| Privacy and metadata | Allowlisted auxiliary metadata, filtered labels, source envelopes, refilter publication, parser/source version changes, and privacy-sensitive predecessor cleanup. |
| Retention and purge | Ownership and destination boundaries, clock verification, metadata-before-source deletion, fail-closed inventories, fresh checks before deletion, resumable reports, and explicit pause requirements. |
| Local concurrency | Staging outside request locks, conflict retries, hook-specific recovery, retention/request races, corruption quarantine, and registration removal rollback. |
| Linux credentials and scheduling | Private file permissions and ownership, no-follow reads, bounded credential files, platform adapters, systemd artifact ownership and recovery, and refusal of unsafe shared data directories. |
| Setup and backfill | Reconfiguration locks, concurrent settings detection, journal rollback, stranded-job recovery, guided bucket rollback, imported-session ownership, and undo boundaries. |
| Browsing and handoff | Scope and query matching, terminal key handling, cancellation, repo trust, worktree paths, launch argument escaping, noninteractive behavior, pager failure recovery, and output permissions. |
| Statistics | Session/window semantics, parent and child attribution, usage units, unknown prices, HTML escaping, CSP, and anonymization. |
| Packaging | Four asset names, checksum/attestation/signing lists, pinned workflow actions, tag guards, installer verification, and release version embedding. |

The absence of another confirmed finding is not a substitute for the live acceptance checks below.

## Validation results

Validation was run with Go 1.27.1 on macOS amd64. The initial cache-restricted attempts were superseded by a run with access to the normal Go caches. A duplicate temporary-cache run was stopped to avoid running the same expensive race suite twice.

| Check | Result |
| --- | --- |
| `go vet ./...` | Passed. |
| `go test -race -timeout 20m ./...` | The full run did not pass: CLI reached its 20-minute package deadline and a git subprocess test exceeded its short timeout under load. The git test and terminal-echo test passed individually afterward. The other packages passed with no reported race. An isolated CLI rerun was still active when the turn was interrupted; its temporary log and process handle were unavailable afterward, so its final result is unverified. A clean full race run remains required before release. |
| Python script tests | 163 tests: 159 passed, 3 skipped, and 1 sandbox error binding a loopback socket. The socket-dependent test passed with the required access on rerun, giving 160 passes and 3 skips across the runs. |
| Documented non-race performance assertions | All named checks passed across runs. 300 unchanged sessions passed at 572 ms with no writes; the large-record checks passed; the Cursor 3-hook slow-write burst had no missing registrations and p95 1.155 s. Growing 30 MB transcripts failed timing budgets under competing build/race load, then passed after those jobs finished: 4.569 s first publication, 4.143 s republication, 1.162 s metadata refresh. |
| Four-platform release build | Passed for darwin/amd64, darwin/arm64, linux/amd64, and linux/arm64 with `VERSION=v0.2.0-review`. All four checksums verified. Native macOS execution reports that version and all foreign binaries embed it; `file` identifies both Linux binaries as statically linked ELF executables. Foreign binaries were not executed locally, and the review artifacts are unsigned. |
| Mixed-version index regression | Failed as expected: indexed result omits the new legacy session and claims completeness. |
| Stats no-force fault injection | Failed as expected: the exporter returned success and replaced `existing report` with `new report` without force. |
| Newest pager regression | `TestPagerFallsBackOnlyWhenItCannotStart` passed with the race detector on the latest source. |
| Targeted privacy fuzzing | `FuzzSubagentMetaDropsSecrets` passed a 10-second fuzz run with two workers and 3,129 executions. This supplements the committed fuzz corpus, not the complete CI fuzz job. |
| Changes landing during review | Final `98f8bd0` passes `go vet`, the newly added collector-helper test, and full race suites for `cursorstore` and `scheduler/systemd`. All four review binaries were rebuilt from this commit and their checksums and embedded versions reverified. |
| Candidate CI | Latest candidate run queued at inspection; prior production commit `61d068b` has a [successful Test run](https://github.com/wangjohn/agent-archive/actions/runs/36892781543). Require successful CI on the final release commit. |

The script skips were the obsolete missing-team installer case (the team is now configured), optional ShellCheck when unavailable, and a Linux-native execution case on macOS. The repro tests use temporary Go overlays and synthetic data. They are separate from the unchanged repository test suite. Local builds are unsigned review artifacts, not published-release acceptance evidence.

## Fix validation on fc868ae plus local changes

- Focused CLI regression tests passed with `-race` (27.822 seconds), including
  clipboard provider execution, missing providers, headless menus, daemonized
  selection ownership, destination races, unsupported hard links, and existing
  HTML write and handoff behavior. Tests use synthetic data and temporary fake
  executables, never the real clipboard.
- `go vet ./...` passed.
- Full reader and platform package tests passed (32.170 seconds and 2.380 seconds).
- CLI reference and quoted command/flag documentation tests passed (1.674 seconds).
- Linux arm64 cross-build passed. This is build evidence, not native Linux execution.
- `git diff --check` and changelog release-baseline/structure checks passed.
- The earlier incomplete full race run is not superseded by these focused checks;
  require passing full candidate CI before tagging.
- GitHub protection remains pending owner reauthentication after selecting
  `cross-build` and `real-systemd`. The browser is left at Confirm access.

## Remaining acceptance evidence

Use [the release runbook](releasing.md), [published-release acceptance sheet](open-source-acceptance.md), and [Linux live acceptance recipe](../contributing/testing.md#the-linux-live-acceptance-run). These checks require disposable accounts/buckets or the actual release assets. They were not executed against the user's real environment during this review. The newly committed Linux record and its available local summary establish 91 passing live harness checks on Ubuntu 24.04.5/systemd 255, amd64, at `61d068b`; prior arm64 runs are also documented. That evidence is valuable but predates the latest cache and working-directory fixes, and excludes real coding-agent applications, AWS/R2, desktop operation, and upgrades. Rerun the relevant live checks on the final candidate:

- Clean install on Intel macOS, Apple Silicon, and Linux; capture, sync, browsing, stats, and handoff in each enabled harness.
- Linux user systemd timer execution, logout/reboot behavior, recovery, credential storage, and uninstall using the live acceptance recipe.
- S3 and R2 under the documented narrow permissions, including read-back, absence detection, retention, and privacy cleanup. Guided AWS bucket creation needs provider acceptance; fake clients cannot prove real policy behavior.
- Owner evidence for the protected `release` environment and signing-secret scope, private vulnerability reporting, and issue labels. These remain unverified where the connector did not expose the settings.
- After tagging: all five assets, embedded version, checksums, macOS signing/notarization, and verified provenance for all four binaries. Record tester, UTC date, final SHA, and evidence links.

## Suggested release sequence

1. Preserve the documented current-writer index contract; no legacy migration work is needed.
2. Review the completed export and clipboard fixes and permanent tests.
3. Review the consolidated Unreleased notes for the planned v0.2.0.
4. Require the two missing CI jobs and wait for all candidate checks on the final SHA.
5. Complete disposable-environment acceptance, record accessible settings, and confirm the experimental R2 gate remains intact.
6. Follow the release runbook to tag and publish, then verify the downloaded assets and update the acceptance record.
