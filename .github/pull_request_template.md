## What and why

<!-- What this changes, and the problem it solves. Link the issue if there is one. -->

## How it was tested

<!-- Tests added or changed, and anything checked by hand. Never test setup or
uninstall against your real HOME: use AGENT_ARCHIVE_HOME, a temporary HOME,
and a stubbed launchctl. -->

## Checklist

- [ ] CI's Linux race, macOS smoke, cross-build, macOS lint, and Levenshtein checks pass (see `dev/contributing/testing.md`). Run the Extended workflow on this branch when changing macOS integration, fuzz targets, or systemd behavior.
- [ ] Every bug fix has a regression test that fails without the fix.
- [ ] New or changed exported identifiers have doc comments (CI runs revive on new code).
- [ ] **Privacy:** if this changes what the filter keeps, drops, or redacts (anything that changes uploaded bundle content), `FilterVersion` is bumped (and the adapter version if adapter output changed), `dev/specs/privacy-filter-changelog.md` has a section for it, and the golden files are regenerated.
- [ ] If metadata derivation changed, `DefaultParserVersion` is bumped and the schemas under `schemas/` still match.
- [ ] User-facing behavior, commands, or output changed: docs (README, the guides under `docs/`, help text, CHANGELOG.md) are updated.
- [ ] No transcript content, credentials, or personal paths appear in tests, fixtures, logs, or this description.
