# Changelog

All notable changes are recorded here, by what they mean for you. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
will follow [Semantic Versioning](https://semver.org/) from `v0.1.0`. There
is no tagged release yet: build from source (see the
[install guide](docs/getting-started/install.md)).

## [Unreleased]

### Security

- **Privacy filter 9** (#35). Credentials are redacted by any common name:
  `DB_PASSWORD=`, `AWS_SECRET_ACCESS_KEY=`, `OPENAI_API_KEY=`, JSON and YAML
  keys, `--token` flags, and more; only the value is replaced, so you can see
  which credential was there. Pasted images, PDFs, and other base64 content
  are no longer archived. Structured Cursor tool results are filtered like
  everything else. A skill file that is a symlink out of its project (for
  example to `~/.aws/credentials` or the project's `.env`) is no longer read.
  Sessions whose transcripts are still on your Mac are refiltered and
  republished automatically; earlier snapshots are not rewritten. See the
  [filter changelog](docs/security/filter-changelog.md#source-filter-version-9).
- **Hook files are edited in place** (#37). Setup changes only the `hooks`
  entry of each app's settings file, keeping every other setting's text,
  order, and numbers; uninstall restores a file byte for byte. A settings
  file that is a symlink (stow, chezmoi) is updated at its target and the
  link is kept.

### Added

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
- Backfill: undoing an import keeps a project that another import still has
  sessions in, and says so; the undo of the last such import excludes it.
  Ctrl-C while the plan is being made stops cleanly (#42).

### Fixed

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

- Lint (golangci-lint), `govulncheck`, Dependabot, SHA-pinned Actions, and
  issue and PR templates (#38).
- Documentation reorganized under `docs/` with an index, a threat-model-first
  privacy page, reference pages, and a link check that runs with the tests.
