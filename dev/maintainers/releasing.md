# Releasing

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml` in three jobs:

1. **build** (macOS runner, read-only token, no secrets). Refuses a tag that
   isn't exactly `vX.Y.Z` or whose commit is not on `main`
   (`git merge-base --is-ancestor`), runs `go vet`, `go test -race ./...`,
   and the script tests, cross-builds `darwin/amd64` and `darwin/arm64`
   with `scripts/build-release.sh darwin` (the same script contributors
   run), and checks the embedded version.
2. **build-linux** (`ubuntu-24.04`, read-only token, no secrets). The same
   tag and `main` guard and the same tests, then cross-builds `linux/amd64`
   and `linux/arm64` with `scripts/build-release.sh linux` (pure Go,
   `CGO_ENABLED=0`, statically linked). `scripts/verify-linux-release.sh`
   checks that both are static, runs the amd64 binary natively and requires
   its `--version` to equal the tag, and checks that the arm64 binary (which
   cannot run on the runner) embeds the tag.
3. **publish** (macOS runner, needs both builds, runs in the `release`
   environment, the only place the Apple secrets exist). Checks the signing
   configuration, codesigns and notarizes the two macOS binaries, checks
   them against the same Developer ID requirement `install.sh` uses,
   computes `SHA256SUMS` over all four binaries
   (`scripts/write-checksums.sh`), records a
   [build provenance attestation](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations)
   for all four, and publishes them with `SHA256SUMS`.

The release therefore has five assets: `agent-archive-darwin-amd64`,
`agent-archive-darwin-arm64`, `agent-archive-linux-amd64`,
`agent-archive-linux-arm64`, and `SHA256SUMS`. The Linux binaries are
**unsigned**: Apple signing and notarization apply to the macOS binaries
only, and Linux has no equivalent step. Trust in a Linux binary rests on the
checksum in `SHA256SUMS` (integrity only, since both files come from the same
release) and on the GitHub build attestation, which ties the exact bytes to
this workflow run and commit.

The attestation subjects, the workflow artifact upload, and the
`gh release create` file list must all name exactly the released binaries;
`scripts/test_release_assets.py` fails when they disagree, or when a Linux
binary reaches the codesign or notarize steps. When you add or rename a
release asset, change `scripts/write-checksums.sh`, those three lists, and
that test together.

The workflow runs only on a tag and has no manual trigger, so it is not
exercised by pull requests. The [Test workflow](../../.github/workflows/test.yml)'s `cross-build` job runs the
Linux half of it (`scripts/build-release.sh linux` and
`scripts/verify-linux-release.sh` on a placeholder version) on every pull
request. To rehearse the rest, push a `vX.Y.Z` tag on a commit in `main` to
a fork: `build` and `build-linux` run in full, and `publish` stops at its
signing-configuration check because the fork's `release` environment has no
Apple configuration. Never work around that stop to publish from a fork.

The tag name reaches scripts only through the `VERSION` environment
variable, never expanded into a script's text.

Update [CHANGELOG.md](../../CHANGELOG.md) before tagging; the release notes
point at it and at the [install guide](../../docs/getting-started/install.md).

## Repository settings and release evidence

Before tagging, an owner checks the live GitHub settings and records **checker,
UTC date, tag, commit SHA, result, and evidence link or screenshot** for each
item in the release record. Mark inaccessible checks **unverified**, not
absent. Workflow files and issue templates describe intent; they do not prove
the corresponding settings or labels are active.

| Check | Live evidence to record |
| --- | --- |
| `release` environment | Settings → Environments shows required reviewers, deployment tag restriction `v*.*.*`, `APPLE_SIGNING_ENABLED=true`, and the six Apple secrets scoped to this environment (names and scope only; never record values). |
| `main` protection | Settings → Rules → Rulesets or Branches shows protection enabled and the exact required pull-request CI checks for `main`; compare their names with [Test](../../.github/workflows/test.yml) and [Levenshtein](../../.github/workflows/levenshtein.yml). Require `linux-race`, `macos-smoke`, `cross-build`, `lint`, and `verify` after confirming their exact displayed check names. Coordinate the switch with the CI workflow merge: retire `test (ubuntu-latest)`, `test (macos-14)`, and `fuzz`, then refresh other open PR branches so they emit the new checks. |
| Post-merge validation | Record a passing [Extended workflow](../../.github/workflows/extended.yml) on the release commit, plus fuzz and real-systemd runs on that SHA (dispatch them manually when the latest nightly ran on an earlier commit). Triage a red Extended run before tagging. |
| Private vulnerability reporting | Settings → Security → Code security and analysis shows private vulnerability reporting enabled; test the private reporting route described in [SECURITY.md](../../SECURITY.md) without submitting a real report. |
| Issue labels | Confirm `bug`, `capture-gap`, and `enhancement` exist in live repository labels and match the [issue templates](../../.github/ISSUE_TEMPLATE/). |
| Published tag | Confirm the release tag points to the tested `main` commit, the publish job completed signing and accepted notarization for both macOS architectures, and the release has `agent-archive-darwin-arm64`, `agent-archive-darwin-amd64`, `agent-archive-linux-arm64`, `agent-archive-linux-amd64`, and `SHA256SUMS`. Download all five; verify the macOS signatures and every checksum (`shasum -a 256 -c SHA256SUMS` on macOS, `sha256sum -c SHA256SUMS` on Linux). The Linux binaries have no signature to verify. |
| Provenance | Verify all four downloaded binaries with `gh attestation verify <file> --repo wangjohn/agent-archive`; record the verified subject digests and workflow run. |

On 2026-09-28, the GitHub connector showed the repository public and a
published `v0.1.1` release with both architecture assets and `SHA256SUMS`.
Its branch-protection request returned HTTP 403 and the rulesets collection
was empty. Those responses leave live `main` protection **unverified**; they
do not establish that protection is absent. The other settings and the
signing, notarization, and attestation checks above still need their own live
evidence.

## Before the first release

These are repository settings only the owner can make:

- Create the `release` environment (Settings → Environments) **before the
  first tag**: a job that names an environment that doesn't exist gets one
  created for it, with no protection. Add required reviewers, limit its
  deployment branches and tags to the tag pattern `v*.*.*`, and move the
  Apple secrets below into it, deleting the repository-level copies.
  Without reviewers, anyone who can push a tag to a commit on `main` can
  publish.
- `team_id` in `install.sh` is the Apple Developer Team ID that signs
  releases, `568CGRV32C`. The `APPLE_TEAM_ID` secret must be the same value
  (shown under Membership details at developer.apple.com):
  `scripts/check-release-signing.sh` refuses to publish while they differ.
- Protect `main` so the tag's commit has passed CI.

## Signing and notarization

Release binaries are codesigned with a Developer ID Application certificate
and submitted to Apple's notary service, so Gatekeeper can verify them
without a manual approval step. This needs an Apple Developer Program
membership and these secrets in the `release` environment:

`APPLE_CERTIFICATE_P12_BASE64`, `APPLE_CERTIFICATE_PASSWORD`,
`APPLE_SIGNING_IDENTITY`, `APPLE_ID`, `APPLE_TEAM_ID`, and
`APPLE_APP_SPECIFIC_PASSWORD`.

Also set the variable `APPLE_SIGNING_ENABLED` to exactly the lowercase
string `true`; any other value (`True`, `TRUE`, `1`, `yes`) counts as
disabled. A tagged release fails before signing if the flag or any secret is
missing, or if `install.sh` names another team. Signing, signature
verification, and accepted notarization are required before either
architecture is published. Never put these values in repository files.

`scripts/check-release-signing.sh` and its test
(`scripts/test_release_signing.py`) check that the workflow fails closed.
Local builds from `scripts/build-release.sh` are unsigned and for
development only; they report `dev-<commit>` from `--version`. The Linux
binaries are never signed, in a release or locally; the secrets above do not
apply to them.

## What a user can verify

- `install.sh` checks the download against the release's `SHA256SUMS`
  (integrity only: both files come from the same release) and then requires
  a strict code signature from a Developer ID certificate issued to the
  pinned team (`codesign --verify --strict -R=…`).
- Anyone can check provenance by hand, for any of the four binaries:
  `gh attestation verify agent-archive-darwin-arm64 --repo wangjohn/agent-archive`
  (or `agent-archive-linux-amd64`, and so on).
- A Linux binary has no code signature, so its trust is the checksum plus the
  attestation. Check the download against the release's checksums with
  `sha256sum -c --ignore-missing SHA256SUMS` (Linux) or
  `shasum -a 256 -c --ignore-missing SHA256SUMS` (macOS, whose `shasum`
  accepts the same flag), then run the `gh attestation verify` command above.
  Do this before running a downloaded Linux binary. A downloaded release
  asset is not executable, so `chmod +x` it after the checks pass.

## Versions

A release that changes what is filtered or how metadata is derived must
already carry the matching version bumps; see [versions](versions.md).

## Release-time documentation audit

For every tag, check the [README](../../README.md) commands and synthetic
output against that tag's CLI, confirm pinned install examples name a
published tag and its installer script, and review the dated evidence in
[tested app versions](../../docs/reference/capture-capabilities.md) before
changing any version claim. Recheck the Cloudflare and AWS dashboard steps in
the [bucket guide](../../docs/getting-started/bucket.md), follow its current
provider pricing link, and open external links in the release-facing docs.
Run the repository's relative-link and generated CLI-reference checks too;
passing those checks does not establish that external URLs work. Record the
documentation commit and any pending live checks beside the release evidence.
