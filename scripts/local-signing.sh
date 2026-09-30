# Sourced by build-release.sh and install-from-source.sh; not run directly.

# release_asset_name prints the file name of a release binary for an OS
# (darwin or linux) and architecture, as install.sh downloads it. A macOS
# release's code signing identifier is its file name (see release.yml), and
# development builds are signed with the same identifier, so the name is
# spelled here once.
release_asset_name() {
  printf 'agent-archive-%s-%s' "$1" "$2"
}

# sign_local_build signs a development build with a stable identity, so a
# Keychain "Always Allow" survives rebuilds. The Go linker signs ad hoc, and
# an ad hoc binary is identified to the Keychain by its hash alone: every
# rebuild is a new program and is asked again. A certificate identity plus a
# fixed identifier is not. Signed with the project's own Developer ID, a
# build matches the release's designated requirement, so a Keychain item that
# already trusts the release trusts it too; any other identity is trusted
# after one "Always Allow".
#
# That trust extends to every build signed this way, including one from a
# branch you have not reviewed, so it is opt-in: AGENT_ARCHIVE_SIGN_IDENTITY
# is "auto" (a Developer ID Application identity, else the first valid one),
# an identity's name or SHA-1 hash, or unset or "-" for the ad hoc signature.
# A signing failure (a locked Keychain over SSH, say) keeps the ad hoc
# signature with a warning rather than failing the build.
sign_local_build() {
  local binary=$1 identifier=$2 identity="${AGENT_ARCHIVE_SIGN_IDENTITY:--}" identities
  [[ "$identity" != - ]] || return 0
  if [[ "$identity" == auto ]]; then
    identities="$(security find-identity -v -p codesigning 2>/dev/null || true)"
    identity="$(sed -nE 's/^ *[0-9]+\) ([0-9A-F]{40}) "Developer ID Application: .*/\1/p' <<<"$identities" | head -n 1)"
    [[ -n "$identity" ]] || identity="$(sed -nE 's/^ *[0-9]+\) ([0-9A-F]{40}) ".*/\1/p' <<<"$identities" | head -n 1)"
    if [[ -z "$identity" ]]; then
      printf 'Warning: AGENT_ARCHIVE_SIGN_IDENTITY=auto found no code signing identity; %s stays signed ad hoc.\n' "$binary" >&2
      return 0
    fi
  fi
  # --timestamp=none: a Developer ID signature otherwise asks Apple's
  # timestamp server by default, and would fail offline.
  if ! codesign --force --timestamp=none --sign "$identity" --identifier "$identifier" "$binary"; then
    printf 'Warning: could not sign %s with identity %s; it stays signed ad hoc.\n' "$binary" "$identity" >&2
  fi
}
