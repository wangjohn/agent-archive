# Changelog

All notable changes are recorded here, by what they mean for you. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `handoff --to claude|codex|cursor` launches a local coding agent with the
  filtered session record. The receiving agent is told how to inspect the
  archived or current local record with Agent Archive. The record is kept in
  the data directory's `handoffs/` for 7 days, so a resumed session can read
  it again (before setup, in a private temporary folder the system clears). Each launch's copy has a folder of its own, which is all
  Claude Code gets with `--add-dir`; Codex and Cursor get the checkout with
  `--cd` and `--workspace`. Cursor's `agent` CLI is tried before
  `cursor-agent`. The
  launched agent does not inherit the calling agent's session variables.
- Arguments after `--` go to the agent `handoff --to` launches, and
  `config.json` may set per-agent arguments (`handoff.args`) and a default
  destination per source harness (`handoff.default_to`).
- `show --transcript` prints a session's conversation to read: each prompt,
  the agent's replies, one line per tool call (✗ when it failed), your `!`
  shell and local slash commands, compactions, and app notices such as a
  background task finishing, paged on a terminal (`--no-pager` to print
  directly). `--full` adds tool results and shell output, trimmed.
  `--transcript --json` prints what `--normalized` printed.
- Browsing on a terminal (`list`, bare `show`) opens a session's summary in
  place of the list, on the terminal's alternate screen: `t` shows its
  transcript, Enter or `b` goes back to the list, and `q` quits. The last
  summary viewed stays in scrollback. Bare `show` now keeps browsing like
  `list` instead of exiting after one pick.
- Metadata may include optional `ended_at` (latest record timestamp),
  `tools_used` (the 10 most-called tools with counts), and
  `counts.files_touched` (distinct files edited; a count only, never
  paths). Parser version is now `0.13.0`, so existing sessions gain them on
  the next metadata refresh.
- A Claude Code parent session whose subagent's transcript was never written
  now says why the subagent is missing: its metadata carries a
  `subagent_transcript_never_written` capture gap, "Claude Code reported a
  subagent but never wrote its transcript".
- `status --verbose` counts the subagents dropped in the last 7 days because
  Claude Code never wrote their transcripts, by the type Claude Code
  reported ("7 not archived in the last 7 days (Claude Code never wrote
  their transcripts; nothing to do)", then "5 unknown type, 2 Explore"), and
  `status --json` lists them as `collector.expired_subagents`. The type is
  kept on this Mac only and never uploaded. Default `status` still says
  nothing about them.

### Changed

- **Breaking for scripts:** `show SESSION_ID` now prints a readable summary
  (title, when, app, models, activity, skills, subagents, capture gaps)
  instead of JSON. Pass `--json` for the metadata sidecar, byte for byte what
  `show` printed before.
- `show --normalized` is deprecated in favor of `show --transcript --json`.
  It still works, with unchanged output, and prints a note on stderr.
- `handoff` shows file paths the same way everywhere: relative to the
  workspace root (`./a.go`, `../repo/a.go`, and `/repo/a.go` are all
  `a.go`; a relative path above the root, such as `../other/b.go`, keeps
  that spelling; the root itself is `.`; Windows drive paths use forward
  slashes), and its files-touched list no longer repeats one file under two
  spellings. A search of the workspace root no longer ends in
  `in <root>` (or `in .`). A Codex `local_shell_call` is listed even when
  nothing about it was retained, and `tools_used` counts exactly the calls
  `handoff` lists. It also recognizes Cursor's `edit_file_v2`, `read_file_v2`,
  `ripgrep_raw_search`, and `glob_file_search` tools, so edits in chats
  imported from Cursor's database are listed and counted in
  `counts.files_touched`.

### Fixed

- A Claude Code subagent whose transcript is never written (Claude Code
  reports some background agents that way) no longer fails every pass:
  `sync` exited 1 and `status` reported "N session(s) need capture or
  publication" for good. Such a subagent now waits up to 30 minutes for its
  transcript, then is dropped and its parent session records the link as
  unavailable. `sync` counts waiting subagents, and ones it decided not to
  capture, without failing ("; N subagent(s) waiting for transcripts",
  "; N subagent(s) not captured"), and `status --verbose` shows how many are
  waiting. A subagent `backfill` found without a transcript is dropped at
  once. A subagent that is lost for another reason (its transcript exists but
  can't be read, or doesn't match its parent) is dropped too, and reported as
  a failed session by that pass only.
- `status` names what kind of failure kept sessions from syncing, and what to
  do about it, instead of an opaque count ("N session(s) need capture or
  publication", or "failed to scan, publish, or clean up" when cleanup also
  failed). The last error now reads, for example, "2 sessions failed to
  reach storage (network or service unavailable) — check the network and
  the storage service, then run agent-archive sync (the next pass also
  retries)", counts subagents apart from sessions, and covers collection
  and retention failures of the same pass in one message. `sync` prints
  each of a session's errors on its own line. Each session's capture gap in
  `status --json` carries its own next step. When no failed session is
  about storage, status no longer leads with "The last sync failed / Check
  storage access": it names the kind ("Some sessions could not be
  captured") and its next step, or leads with no failure, and shows the
  problems as information rather than ✗ rows, when there is nothing to do
  (a subagent that could not be captured, sessions over the transcript
  size limit).

### Changed

- `status --json` adds `collector.issue_counts`, the number of sessions per
  failure code. The fallback code in `collector.session_issues` is now
  `capture_failed` (was `capture_or_publication_failed`, which older status
  files still carry and status still reads), and new codes name storage
  credential, storage availability, retention failures, and subagents that
  could not be captured (`subagent_not_captured`).

## [0.2.0] - 2026-09-29

The archive browser now has bounded, readable listings and terminal pickers for
`list`, `show`, and `handoff`. Scripts should update consumers of `list --json`
to schema version 4, which reports whether the total match count is known.

### Added
- `list --limit N` caps how many sessions are shown (default 50, newest
  first; `0` for all). A truncated text listing reports
  `Showing N or more session(s)` when the count is unknown, or
  `Showing N of M session(s)` when exact; `list --json` is now `schema_version` 4
  and includes `limit`, `returned`, and `total_matched_known`; it includes
  `total_matched` only when the count is exact and `truncated` when the
  result was cut short.
- On a terminal, `list` (text only) pages through `$AGENT_ARCHIVE_PAGER`,
  else `$PAGER`, else `less -FRX`. Use `--no-pager`, or set either env var
  to empty or `cat`, to print directly. `--json` is never paged.
- Default `list` text is title-first: first filtered prompt preview (or a
  short SESSION_ID when none), relative capture time, harness, project, and
  short ID. `--verbose` restores full IDs, absolute times, origin, parser
  status, and all models/skills.
- On an interactive terminal (stdin and stdout), `list` and bare `show`
  offer a numbered picker to print a session's metadata; `q` quits. Piped
  output and `--json` stay non-interactive. Bare `agent-archive` on a TTY
  opens the same interactive list when already set up.
- Bare `handoff` uses the same numbered picker to select an archived session
  on a terminal. Scripts still supply a session ID, `--latest`, or `--file`.
- `show` accepts a title substring (and short SESSION_ID) when the argument
  is not an exact id; multiple matches use the picker on a TTY.
- Human `list` groups rows under project headings when more than one project
  appears in the page.
- Metadata may include optional `title` (preview of the first filtered human
  prompt) and `project_name` (project basename at publish). Parser version
  is now `0.12.0` so existing sessions get titles on the next metadata
  refresh.
- `status --json` has `collector.last_errors`: each problem the last pass
  recorded, one per entry. `collector.last_error` is unchanged (the same
  problems joined with `; `).

### Changed

- Long-running CLI steps show a short TTY spinner (registering sessions,
  finishing upload, waiting for the collector, scanning, listing, loading a
  session, looking for past sessions, checking storage). Piped and CI output
  stay plain.

### Fixed

- `status` shows each problem the last pass recorded on its own ✗ row, and
  a storage provider's error message containing `; ` is no longer split in
  two or shown as the wrong cause.
- A sync's problems no longer hide each other in `status`: a read-back or
  retention failure after collection is shown beside the problems the pass
  already recorded (such as a session over the size limit, or retention
  held by the clock) instead of replacing them.
- `status --verbose` prints each of the last pass's problems on its own
  `Last error:` line.

## [0.1.1] - 2026-09-28

Setup and status UX polish since the first tagged commit. No filter or
bucket-layout changes.

### Added

- A shared terminal style for setup and status: green ✓, yellow !, red ✗,
  cyan commands, dim details, and your home folder shown as `~`. Plain
  output with `NO_COLOR`, `TERM=dumb`, or when piped is unchanged.
- Golden transcripts of the setup and status screens, in color and plain, so
  changes to what you see are reviewed line by line.
- A plain-words diagnosis for storage failures (no credentials, access
  denied, no such bucket, wrong region, network), each with a fix to try.
  The AWS SDK no longer prints its own warnings to the terminal.
- A read-only check of each app's hook settings file that reports the file,
  line, column and reason for JSONC comments, trailing commas and duplicate
  keys, without changing anything.
- `setup` checks before the first question that each app's hook file is
  valid, that `launchctl` responds, and, when R2 is used, that the Keychain
  opens. A problem stops setup with the file and line to fix.
- At the end of `setup`, an offer to import the past sessions of the
  projects you chose, and a copy-paste `agent-archive setup --yes …` line
  for setting up another Mac.

### Changed

- `setup` suggests `AWS_PROFILE` when it is set, marks AWS profiles that
  have no credentials configured, and defaults to S3 when a usable profile
  exists (R2 otherwise).
- `setup` always shows your recent projects with each one's session count,
  with the current repository pre-selected; `a` selects them all, and
  leaving every project out asks again instead of ending setup.
- `setup` lists the chosen AWS profile's buckets to pick from (an
  `agent-archive*` bucket is pre-selected) and uses the bucket's own region.
  A region that isn't shaped like one is refused, and a saved one of that
  kind is dropped on reconfigure.
- When the storage check fails, `setup` shows the cause once with its fix
  (raw error text only with `--verbose`), the menu defaults to fixing the
  answer that failed, and "Continue where you left off" asks that answer
  again instead of re-running the same check.
- `setup --yes` reports every missing or wrong answer together, one per
  line with the flag that fixes it, instead of stopping at the first.
- `setup` questions and their defaults are bold with a `›` cursor, every
  command to type is cyan, and the storage check shows a spinner that ends
  in ✓ or ✗ on the same line (none when a `credential_process` may prompt).
- The `setup` review is a short list of settings (storage as one `s3://` or
  R2 address) followed by a ✓/!/✗ checklist: storage connected, bucket
  private, hook files valid, and each app's next step. A ✗ blocks starting
  until it's fixed.
- `status` is redesigned: a colored state line, the one fix to make on top,
  and grouped Capture and Storage rows with ✓/!/✗, relative times and `~`
  paths. Internal codes and exact times stay in `status --json`, which is
  unchanged.
- `status --verbose` adds a Details section with the internal codes, exact
  times, bucket privacy evidence and per-app notes, and the default view
  names a storage failure's cause in plain words instead of SDK text.
- `setup` polish: "Nothing, exit" leaves no draft behind, a single-area edit
  drops "Step n of 3", the retention warning shows the local date and only
  appears when sessions are affected, hook-file errors name the real
  problem, and `setup --yes` refuses a public bucket like interactive setup.

## [0.1.0] - 2026-09-25

The first release.

### Added

- **Capture** of Claude Code, Codex, and Cursor sessions on macOS through
  each app's lifecycle hooks, for the projects you include. A LaunchAgent
  filters each session, uploads it with its metadata to your own S3 or R2
  bucket, verifies what arrived by its checksum, and deletes sessions past
  the retention period (90 days by default). A long, growing session costs
  a pass a few seconds, and nothing is downloaded to verify it.
- **`setup`**: three steps (apps and projects, storage, review), resumable if
  interrupted, and safe to rerun to change any of them. It offers the
  projects your apps already have sessions in, accepts the R2 bucket URL the
  Cloudflare dashboard shows, and ends with what each app needs next.
  `setup --yes` takes its answers as flags, to script a second Mac. R2
  secrets are kept in the macOS Keychain; S3 uses an AWS profile, and the
  collector runs with the AWS settings files, CA bundle, endpoint overrides,
  proxy settings and `PATH` setup verified it with (never AWS keys or tokens), so a
  `credential_process` like `aws-vault` or `op`, and their settings for
  where credentials live, work in the background too; `status` says when
  they stop working. Setup refuses a temporary
  binary, such as the one `go run` deletes on exit, and after a plain
  `uninstall` it sets up again with your saved answers.
- `agent-archive` with no arguments says when setup hasn't run yet.
- **`status`**: storage, collector, hook, and per-app capture health, with a
  `Next:` line whenever something needs you. `--json` for scripts.
- **`list`, `show`, `handoff`**: browse the archive from any Mac sharing the
  bucket, and print a session as a prompt another agent can continue from.
- **`backfill`**: import sessions already on this Mac after showing a plan;
  `backfill history` and `backfill undo` review and remove imports.
- **`sync`, `pause`, `resume`, `feedback`, `uninstall`**.
- **`install.sh`**, a one-line installer that checks the release checksum
  and Developer ID signature.
- A [CLI reference](docs/reference/cli.md) generated from the CLI, a
  [glossary](docs/reference/glossary.md), and
  [JSON schemas](docs/reference/schemas.md) validated against real output.

### Security

- **Privacy filter 12.** Hidden reasoning, system instructions, binary
  content, and unknown fields are never uploaded. Credentials are redacted
  by name (`DB_PASSWORD=`, `"api_key":`, `--token`, form fields labelled
  password or PIN) and by shape (AWS, GitHub, Slack, Stripe, GitLab, Google,
  Hugging Face, npm, Groq, and xAI tokens; private keys, also
  base64-encoded as in a kubeconfig; Docker registry logins; JWTs; access
  key IDs; wallet seed phrases; passwords in URLs, `curl -u`, `.netrc`, and
  `.pgpass`). Skill hashes are of the redacted text, so they can't confirm a
  guessed secret. See the
  [filter changelog](dev/specs/privacy-filter-changelog.md).
- Hook files are edited in place: only agent-archive's own `hooks` entries
  change, and a file that can't be edited safely is left alone.
- Handoff output is quoted so session text can't escape it or drive your
  terminal.
- Release binaries are built from `main` after the tests pass, then signed,
  notarized, and attested in a separate protected job.

### Known limitations

- macOS only. Redaction is best effort: a secret with no recognizable name
  or shape is archived as it appears.
- R2's S3 keys can't read bucket privacy settings, so `status` reports R2
  privacy as `not_verified`, and setup reminds you to check that public
  access is disabled in the Cloudflare dashboard.
- Sessions are not encrypted by agent-archive; anyone who can read the
  bucket can read them.
- The background collector gets no other shell settings: an S3 profile
  that needs a helper's variables beyond aws-vault's and 1Password's
  reviewed settings, or a proxy with a password in its URL, works for
  `sync` but not in the background; `status` says when the profile's
  `credential_process` fails there. See
  [configuration](docs/reference/configuration.md#environment-variables).

[Unreleased]: https://github.com/wangjohn/agent-archive/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/wangjohn/agent-archive/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/wangjohn/agent-archive/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/wangjohn/agent-archive/releases/tag/v0.1.0
