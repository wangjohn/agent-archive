## What and why

<!-- What this changes, and the problem it solves. Link the issue if there is one. -->

## How it was tested

<!-- Tests added or changed, and anything checked by hand. Never test setup or
uninstall against your real HOME: use AGENT_ARCHIVE_HOME, a temporary HOME,
and a stubbed launchctl. -->

## Checklist

- [ ] `go test -race ./...`, `go vet ./...`, and `golangci-lint run` pass.
- [ ] Every bug fix has a regression test that fails without the fix.
- [ ] New or changed exported identifiers have doc comments.
- [ ] **Privacy:** if this changes what the filter keeps, drops, or redacts (anything that changes uploaded bundle content), `FilterVersion` is bumped (and the adapter version if adapter output changed), the privacy doc's changelog is updated, and the golden files are regenerated.
- [ ] If metadata derivation changed, `DefaultParserVersion` is bumped and the schemas under `schemas/` still match.
- [ ] User-facing behavior, commands, or output changed: docs (README, the guides under `docs/`, help text, CHANGELOG.md) are updated.
- [ ] No transcript content, credentials, or personal paths appear in tests, fixtures, logs, or this description.
