# Changelog

All notable changes are recorded here, by what they mean for you. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
will follow [Semantic Versioning](https://semver.org/) from `v0.1.0`. There
is no tagged release yet: build from source (see the
[install guide](docs/getting-started/install.md)).

## [Unreleased]

The first release, `v0.1.0`, will be cut from this section: agent-archive
captures your Claude Code, Codex, and Cursor sessions on macOS through their
hooks, filters them, and keeps them in an S3 or R2 bucket you own, where you
can list them, inspect them, and hand one to another agent.

### Security

- **Privacy filter 9** (#35). Credentials are redacted by any common name:
  `DB_PASSWORD=`, `AWS_SECRET_ACCESS_KEY=`, `OPENAI_API_KEY=`, JSON and YAML
  keys, `--token` flags, and more; only the value is replaced, so you can see
  which credential was there. Pasted images, PDFs, and other base64 content
  are no longer archived. Structured Cursor tool results are filtered like
  everything else. A skill file that is a symlink out of its project (for
  example to `~/.aws/credentials` or the project's `.env`) is no longer read.
  Sessions whose transcripts are still on your Mac are refiltered and
  republished automatically; earlier snapshots are not rewritten and stay in
  the bucket until the session expires, unless you
  [delete them](docs/security/privacy.md#after-a-filter-upgrade). See the
  [filter changelog](docs/security/filter-changelog.md#source-filter-version-9).
- **Privacy filter 10** (#44). In a Cursor plain-text transcript, only a role
  name at the start of a line starts a section, so an indented `user:` or
  `system:` in tool output (a docker-compose file) can no longer pose as a
  prompt or hide the rest of the transcript. A quoted credential takes along
  whatever a shell would glue onto it (`PASSWORD="abc"realsecret`). As
  with every filter upgrade, older copies stay in the bucket until
  [deleted](docs/security/privacy.md#after-a-filter-upgrade).
- **Releases are gated and signed in a protected job.** A release tag must
  be on `main` and pass the tests before anything is built; signing and
  publishing run in a separate job that alone holds the Apple secrets, and
  each binary gets a build provenance attestation. `install.sh` now also
  checks the Developer ID signature and its team, not only the checksum.
  See [releasing](docs/maintainers/releasing.md).
- **Privacy filter 11.** A password typed through Codex, whose tool
  arguments are a JSON string, or through any form-filling tool
  (`browser_fill_form`, chrome-devtools `fill_form`), is no longer archived;
  any string holding JSON is now filtered as JSON, including Cursor tool
  results stored as strings. A value beside a "Password", PIN, or card
  label is dropped for every tool. Argument names and redaction share one
  credential vocabulary (`X-Api-Key`, `passwd`, `private_key`, `auth` are
  now dropped). New shapes: `curl -u user:pass`, `mysql -p…`, `sshpass -p`,
  `docker login -p`, `.netrc`, cookies, `DB_PASS=`, Azure keys, XML, PGP
  keys, and the token prefixes of Stripe, GitLab, Google, Hugging Face, npm,
  and more. Unquoted values are redacted to the end of the line, YAML block
  values whole, and URL passwords holding `@` or `/` whole; so are the
  string values of a credential-named object or array
  (`"secret": {"value": …}`) and the `value:` of a Kubernetes `env` entry
  named like a credential, also in a file an agent read through a tool that
  numbers its lines. A PEM BEGIN line with no END no longer swallows the
  rest of a file. Sessions whose
  transcripts are still on your Mac are refiltered automatically; earlier
  snapshots stay in your bucket until they expire, unless you
  [delete them](docs/security/privacy.md#after-a-filter-upgrade). See the
  [filter changelog](docs/security/filter-changelog.md#source-filter-version-11).
- **Handoff text can't escape its quoting or drive your terminal.** A bare
  carriage return (progress bars) no longer ends a block quote early, and
  terminal escape sequences (colors, OSC 52 clipboard writes, OSC 8 links)
  and bidirectional overrides are removed from every field of `handoff`
  output, Markdown and JSON. In a Cursor plain-text transcript, a role
  header must be in the case of the transcript's first header (`user:` or
  `User:`), so a line in the other case is content, and a YAML `user:` line
  in tool output no longer starts a Person turn when sections are
  blank-line separated.
- **Handoff output is marked as a record** (#44). Agent text, prompts, and
  summaries are block-quoted, plan items and file names can't add headings,
  and the preamble tells the receiving agent not to follow instructions
  inside the record.
- **Hook files are edited in place** (#37). Setup changes only the `hooks`
  entry of each app's settings file, keeping every other setting's text,
  order, and numbers; uninstall restores a file byte for byte. A settings
  file that is a symlink (stow, chezmoi) is updated at its target and the
  link is kept.

### Added

- `--version` on a build from source prints the commit, `dev-<commit>`
  (with `-dirty` for uncommitted changes), instead of just `dev`.
- A [glossary](docs/reference/glossary.md) of the terms agent-archive uses,
  a [tested versions](docs/reference/capture-capabilities.md#tested-app-versions)
  table, and docs for deleting the archive from the bucket (everything, one
  Mac's sessions, or copies a filter upgrade left behind), a lifecycle rule
  as a backstop, and moving to a new Mac.
- A "Security contact request" issue template, for reaching the maintainer
  privately without describing the problem in public.
- `list --json` prints `{"schema_version": 1, "sessions": [...]}` for
  scripts, and `show` accepts `--json` (#48). See
  [JSON output](docs/reference/json-output.md).
- `status --json` adds `storage_access_confirmed_at`/`_by` (when setup or
  the collector last reached the bucket) and `sessions_with_capture_gaps`
  per app (#48).
- `sync` reports new activity held for the upload interval: `5 waiting for
  the upload interval (next at 14:27)`, instead of counting it as unchanged
  (#49).
- The JSON schemas are typed throughout, list every capture gap code, and
  are validated against real output in the tests (#44). See
  [JSON schemas](docs/reference/schemas.md).
- `agent-archive setup --abandon-recovery`: a way out of an interrupted setup
  when a file changed since (#37).
- `uninstall --yes` for use without a terminal (#37).
- `status` reports collection that is stuck behind a long-held lock, local
  state files it had to quarantine, and sessions whose metadata this version
  can't refresh (#36, #39).
- Setup offers apps found since you last ran it, remembers apps you declined,
  and finds the Codex and Claude Code builds bundled in the ChatGPT and
  Claude desktop apps (#34).
- A one-line installer, `install.sh`, for when releases exist (#33).

### Changed

- **Each installation owns only its own hooks.** A second or test
  installation (`AGENT_ARCHIVE_HOME` set, same `HOME`) used to take over your
  main installation's hooks, so capture silently stopped, and its uninstall
  removed them all. Hooks now belong to the data directory their command runs
  with: setup and uninstall touch only their own, setup refuses to install
  beside another installation's hooks and names it with how to resolve it,
  and `status` reports them (`other_installations`). Existing installs keep
  working unchanged. Only the default installation retires the prototype's
  job and hooks.
- What a command was asked for goes to stdout, and why it did not do it (or
  not all of it) to stderr with exit 1. **Behavior change:** a paused `sync`
  now exits 1 (it exited 0; the background collector is unaffected), and
  per-session sync failures go to stderr. `sync`, `pause`, `setup`, and
  `uninstall` name the command holding the collector lock instead of
  "another sync is already running".
- Uninstall deletes a hook file that removing its hooks leaves empty
  (`{}`, or Cursor's `{"version": 1}` alone), as when setup created it,
  unless it is a symlink. A file that was already `{}` before setup is
  deleted too; to the apps, an empty file and no file mean the same.
- Help: `version --help` shows help instead of failing, `handoff --force` and
  the `show` options have lines of their own, `list --skill-usage` states its
  default, `list --complete` says it also excludes capture gaps, both
  `--since` helps say which day they mean (UTC for `list`, local for
  `backfill`), and the top-level help links the docs.
- The least-privilege S3 policy works whether S3 answers a read of a
  missing object with 404 or 403: on a 403, agent-archive checks with a
  listing of that one key before treating it as missing, and setup's
  connection test now fails, pointing at
  [bucket permissions](docs/security/bucket-permissions.md), if a missing
  object can't be told apart from a denied one. The policy has not yet been
  tested on AWS itself.
- A second installation (a different `AGENT_ARCHIVE_HOME`) gets its own
  launchd label and carries its data directory in its hook commands, so it
  can never stop or replace your main installation's collector. Hooks follow
  `CLAUDE_CONFIG_DIR` and `CODEX_HOME` (#37). Rerun setup once if you use a
  non-default data directory.
- Every command reports argument errors the same way (one line, exit 2),
  every command has its own help, and `--since` accepts a date, a time, or an
  age everywhere (#39).
- `list`, `show`, `handoff`, and `backfill history` print "Not set up" to
  stderr and exit 1 before setup (#37).
- `status --json` leaves out a time that is not known yet instead of printing
  `0001-01-01T00:00:00Z`; a missing time field means "never" (#49). See
  [JSON output](docs/reference/json-output.md#status---json).
- `list` and `show` reject an unknown `--harness` with a one-line error and
  exit 2, as `handoff` and `backfill` do; `list`, `show`, and `handoff`
  accept `claude-code` for Claude (#49).
- Backfill: undoing an import keeps a project that another import still has
  sessions in, and says so; the undo of the last such import excludes it.
  Ctrl-C while the plan is being made stops cleanly (#42).

### Fixed

- `uninstall --delete-local-data` always warned of 0 pending sessions; it now
  counts them, and stops if one registers while you confirm.
- A damaged or newer-version saved setup no longer blocks setup forever:
  setup names it and offers to move it aside. A damaged recovery record is
  named, and `setup --abandon-recovery` moves it aside; a launchctl failure
  during recovery now points to `--abandon-recovery` too.
- `status` no longer fails on one unreadable advisory file (collector status,
  storage health, capture diagnostics, a session's records); it reports the
  rest with a warning naming the file, and a damaged `capture-diagnostics.json`
  now heals. Every error about a damaged `config.json` names it.
- `show`, `list`, `feedback`, `pause`, `resume`, and `sync` before setup no
  longer create the data directory. A Git checkout above the data directory
  (a dotfiles repository at `~`) is named, with `AGENT_ARCHIVE_HOME` as the
  way out.
- Hook files: a key that appears twice inside `hooks` is refused (one copy's
  handlers used to be dropped), a parse error says where and whether it is a
  comment, a trailing comma, or a byte-order mark, a file whose first key
  shares the brace's line gets indented members, and a failed write reports
  one error, not two.
- Docs: "What leaves your Mac" now names the user-level skill folders whose
  `SKILL.md` text every captured session uploads; the Cursor 3.21.13
  read-back and the expiry of Cursor chats that never get a transcript are
  recorded; the documented lint command is the one CI runs.
- Metadata counts (parser 0.10.0, #44): a tool call's arguments no longer
  count as more tool calls, calls come out in a stable order,
  `[Request interrupted by user]` is not a prompt, and Claude Code's
  `<synthetic>` messages are not a model and their usage isn't counted.
  Sessions are re-derived automatically.
- `status` counts sessions with a capture gap rather than gap entries, and
  its `Access:` line no longer says `never` next to a verified
  authentication (#48).
- The collector keeps publishing across a source schema change, isolates one
  unreadable state file instead of stopping the whole pass, tolerates a clock
  that jumped ahead, and no longer hangs on a network that never answers; it
  retries only errors that can succeed (#36).
- Backfill can no longer reuse an import ID, which could make `backfill undo`
  remove an earlier import too, and undo refuses when it can't tell two
  imports apart (#42).
- `list` and `handoff` no longer fail when one session's metadata is deleted
  mid-listing (another Mac's retention) or was written by a newer version; the
  rest are listed with a warning (#42).
- An interrupted backfill plan no longer leaves a copy of Cursor's database in
  the temporary folder for an hour; hooks are never kept waiting while
  backfill reads Cursor's database (#42).

### Internal

- `internal/cli` tests fail closed: `TestMain` gives them a temporary `HOME`
  and stand-ins for launchctl and the Keychain that stop the test, and
  `testEnv` fails every side-effecting call a test did not set up. The
  configured store opens the Keychain through `Env`. Fuzz targets for hook
  file edits and hook command parsing.

- Lint (golangci-lint), `govulncheck`, Dependabot, SHA-pinned Actions, and
  issue and PR templates (#38).
- Local session state moved into its own package, `internal/state`, loaded
  once per collector pass (#43); more tests for backfill (#45).
- Documentation reorganized under `docs/` with an index, a threat-model-first
  privacy page, reference pages, and a link check that runs with the tests.
