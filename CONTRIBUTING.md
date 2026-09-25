# Contributing

Thanks for helping. agent-archive is a small tool with a privacy promise, so
contributions are held to a few firm rules; everything else is ordinary Go.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree
  on the approach.
- A session or field that should have been captured and wasn't: use the
  **capture gap** issue template.
- Vulnerabilities go through [SECURITY.md](SECURITY.md), never a public issue.
- A bug report should say which build you ran: `agent-archive --version`
  prints the release, or `dev-<commit>` for a source build.
- By contributing you agree to follow the [code of conduct](CODE_OF_CONDUCT.md)
  and to license your work under the [MIT License](LICENSE).

## Set up

- Go **1.27.1** exactly: go.mod's `toolchain` line, which any `go`
  command from Go 1.21 on downloads for you. CI refuses any other version.
- macOS with Xcode's command line tools (`xcode-select --install`): the
  Keychain code uses cgo and Security.framework. Linux builds a stub, so most
  tests also run there.
- [golangci-lint v2.14.0](https://golangci-lint.run/) for lint.

What CI blocks on, and how to run the same locally:

```sh
go test -race ./...
go vet ./...
golangci-lint run --disable=revive                                         # the blocking lint run
golangci-lint run --enable-only=revive --new-from-merge-base=origin/main   # doc comments, new code only
python3 scripts/test_release_signing.py && python3 scripts/test_install.py && python3 scripts/test_purge_recipe.py
VERSION=dev ./scripts/build-release.sh
```

plus the shared [Levenshtein](https://github.com/wangjohn/levenshtein)
checks (Go lint and vet, HTTP and SQL rules, modules, `govulncheck`, and
workflow lint and security), which the `verify` job in
[`levenshtein.yml`](.github/workflows/levenshtein.yml) runs at a pinned
commit. The [testing](docs/contributing/testing.md#levenshtein-checks) page
shows how to run them, and a faster native loop for their Go lint.

`go test ./...` includes a check that every relative link in the Markdown
docs resolves. The Python scripts test the release gate, the installer, and
the bucket purge recipes in the docs.

## Never test against your real Mac

Don't run setup, uninstall, sync, or backfill against your real home
directory, your apps' real settings, the real LaunchAgent, your Keychain,
Cursor's real database, or a real bucket while developing. In Go tests, use
the injected `Env` and in-memory store the existing tests use. By hand, use
`AGENT_ARCHIVE_HOME` plus a temporary `HOME` and a stub `launchctl` on
`PATH`: [testing](docs/contributing/testing.md) has the full recipe, and
explains why the stub matters even with a sandboxed `HOME`.

## Privacy-sensitive changes

Anything that changes what leaves the Mac is a privacy change:

- Bump `archive.FilterVersion` and the adapter version, add a section to the
  [filter changelog](docs/security/filter-changelog.md), and update
  [privacy](docs/security/privacy.md) if what is uploaded or dropped
  changes.
- Regenerate the goldens ([how](docs/contributing/testing.md#fixtures-and-goldens))
  and review every changed line in the PR.
- Add fixtures for new shapes, with synthetic content only. Never commit a
  real transcript, even a "harmless" one.
- For redaction or parsing changes, run the fuzz targets for a couple of
  minutes each with `-fuzzminimizetime 2s`, and keep any failing input as a
  seed.

Changes to derived metadata bump `DefaultParserVersion`; schema changes
update `schemas/`. The rules are in [versions](docs/reference/versions.md).

## Pull requests

- Keep each PR to one concern, with a description of what changed, why, and
  the test that proves it. A bug fix includes a test that fails without the
  fix.
- Name tests by the behavior they pin, not the review or ticket that found
  the bug.
- Match the surrounding style: full-sentence comments on exported
  identifiers, errors that say what to do next, no output from hooks.
- Update user-facing docs (README, `docs/`, help text) and
  [CHANGELOG.md](CHANGELOG.md) when behavior changes.
- CI must be green: tests on macOS and Ubuntu, lint, and the Levenshtein
  `verify` job.

## Where things are

The [architecture](docs/contributing/architecture.md) page maps the
packages. Supporting a new coding agent is described in
[adding an adapter](docs/contributing/adding-an-adapter.md). The hidden
`_hook` and `_collect` commands are what app hooks and the LaunchAgent run;
`scripts/measure-hook.py` measures hook latency. Maintainers: see
[releasing](docs/maintainers/releasing.md).
