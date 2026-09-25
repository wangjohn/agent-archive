# Releasing

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml` in two jobs:

1. **build** (read-only token, no secrets). Refuses a tag that isn't
   exactly `vX.Y.Z` or whose commit is not on `main`
   (`git merge-base --is-ancestor`), runs `go vet`, `go test -race ./...`,
   and the script tests, cross-builds `darwin/amd64` and `darwin/arm64`
   with `scripts/build-release.sh` (the same script contributors run), and
   checks the embedded version.
2. **publish** (runs in the `release` environment, the only place the Apple
   secrets exist). Checks the signing configuration, codesigns and
   notarizes both binaries, checks them against the same Developer ID
   requirement `install.sh` uses, records a
   [build provenance attestation](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations),
   and publishes them with `SHA256SUMS`.

The tag name reaches scripts only through the `VERSION` environment
variable, never expanded into a script's text.

Update [CHANGELOG.md](../../CHANGELOG.md) before tagging; the release notes
point at it and at the [install guide](../../docs/getting-started/install.md).

## Before the first release

These are repository settings only the owner can make:

- Create the `release` environment (Settings → Environments) **before the
  first tag**: a job that names an environment that doesn't exist gets one
  created for it, with no protection. Add required reviewers, limit its
  deployment branches and tags to the tag pattern `v*.*.*`, and move the
  Apple secrets below into it, deleting the repository-level copies.
  Without reviewers, anyone who can push a tag to a commit on `main` can
  publish.
- Set `team_id` in `install.sh` to the Apple Developer Team ID that signs
  releases (the `APPLE_TEAM_ID` secret: ten capital letters and digits,
  shown under Membership details at developer.apple.com), in a pull
  request merged before tagging. `scripts/check-release-signing.sh`
  refuses to publish while they differ, and `install.sh` refuses to install
  anything while `team_id` is empty.
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
development only; they report `dev-<commit>` from `--version`.

## What a user can verify

- `install.sh` checks the download against the release's `SHA256SUMS`
  (integrity only: both files come from the same release) and then requires
  a strict code signature from a Developer ID certificate issued to the
  pinned team (`codesign --verify --strict -R=…`).
- Anyone can check provenance by hand:
  `gh attestation verify agent-archive-darwin-arm64 --repo wangjohn/agent-archive`.

## Versions

A release that changes what is filtered or how metadata is derived must
already carry the matching version bumps; see [versions](versions.md).
