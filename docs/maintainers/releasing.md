# Releasing

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`. It cross-builds
`darwin/amd64` and `darwin/arm64` with `scripts/build-release.sh` (the same
script contributors run), checks the embedded version, codesigns and
notarizes both binaries, and publishes them with `SHA256SUMS`. Update
[CHANGELOG.md](../../CHANGELOG.md) before tagging; the release notes point
at it and at the [install guide](../getting-started/install.md).

## Signing and notarization

Release binaries are codesigned with a Developer ID Application certificate
and submitted to Apple's notary service, so Gatekeeper can verify them
without a manual approval step. This needs an Apple Developer Program
membership and these repository secrets:

`APPLE_CERTIFICATE_P12_BASE64`, `APPLE_CERTIFICATE_PASSWORD`,
`APPLE_SIGNING_IDENTITY`, `APPLE_ID`, `APPLE_TEAM_ID`, and
`APPLE_APP_SPECIFIC_PASSWORD`.

Also set the repository variable `APPLE_SIGNING_ENABLED` to exactly the
lowercase string `true`; any other value (`True`, `TRUE`, `1`, `yes`) counts
as disabled. A tagged release fails before building if the flag or any
secret is missing. Signing, signature verification, and accepted
notarization are required before either architecture is published. Never
put these values in repository files.

`scripts/check-release-signing.sh` and its test
(`scripts/test_release_signing.py`) check that the workflow fails closed.
Local builds from `scripts/build-release.sh` are unsigned and for
development only.

## Versions

A release that changes what is filtered or how metadata is derived must
already carry the matching version bumps; see [versions](../reference/versions.md).
