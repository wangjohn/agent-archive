#!/bin/sh
# Installs the latest agent-archive release for this Mac.
#
#   curl -fsSL https://raw.githubusercontent.com/wangjohn/agent-archive/main/install.sh | sh
#
# Downloads the signed, notarized binary for this Mac's architecture from
# GitHub Releases, checks it against the release's SHA256SUMS (which catches
# a damaged download, not a tampered release, since both come from the same
# place), checks that it is signed with this project's Developer ID (which
# does catch a binary someone else built), and installs it as
# `agent-archive`. It never runs setup and never needs sudo.
#
# Environment:
#   AGENT_ARCHIVE_VERSION      release tag to install, e.g. v0.1.0 (default: latest)
#   AGENT_ARCHIVE_INSTALL_DIR  directory to install into (default: where
#                              agent-archive already is, else /usr/local/bin
#                              if writable, else ~/.local/bin)
#   AGENT_ARCHIVE_DOWNLOAD_URL release download base, for testing only
#
# The whole script is one function called on the last line, so a download
# cut off partway through runs nothing.
set -eu

# The Apple Developer Team ID that signs release binaries. The release
# workflow refuses to publish unless it equals the signing team (the
# APPLE_TEAM_ID secret), so the two cannot drift. Empty until the first
# signed release: until then there is nothing this script can install.
team_id=""

main() {
  repo_url="https://github.com/wangjohn/agent-archive"

  if [ "$(uname -s)" != "Darwin" ]; then
    fail "agent-archive supports macOS only (this system reports $(uname -s))."
  fi

  # Before downloading anything: without a pinned team there is nothing
  # this script could accept.
  check_team_id

  arch="$(detect_arch)"
  asset="agent-archive-darwin-${arch}"

  version="${AGENT_ARCHIVE_VERSION:-}"
  if [ -n "$version" ]; then
    case "$version" in v*) ;; *) version="v${version}" ;; esac
    default_base="${repo_url}/releases/download/${version}"
  else
    default_base="${repo_url}/releases/latest/download"
  fi
  base="${AGENT_ARCHIVE_DOWNLOAD_URL:-$default_base}"

  install_dir="$(choose_install_dir)"
  target="${install_dir}/agent-archive"

  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM

  say "Downloading ${asset} (${version:-latest})"
  download "${base}/${asset}" "${tmp}/${asset}"
  download "${base}/SHA256SUMS" "${tmp}/SHA256SUMS"

  expected="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "${tmp}/SHA256SUMS")"
  if [ -z "$expected" ]; then
    fail "SHA256SUMS has no entry for ${asset}."
  fi
  actual="$(shasum -a 256 "${tmp}/${asset}" | awk '{ print $1 }')"
  if [ "$expected" != "$actual" ]; then
    fail "checksum mismatch for ${asset}: expected ${expected}, got ${actual}."
  fi
  say "Checksum verified"
  verify_signature "${tmp}/${asset}"
  say "Signature verified (Developer ID, team ${team_id})"

  mkdir -p "$install_dir"
  # Stage beside the target, then rename over it: replacing the file (a new
  # inode) rather than overwriting it in place keeps macOS from killing the
  # upgraded binary at launch.
  staged="${install_dir}/.agent-archive.install.$$"
  cp "${tmp}/${asset}" "$staged"
  chmod 755 "$staged"
  mv -f "$staged" "$target"

  installed_version="$("$target" --version 2>&1)" ||
    fail "installed ${target}, but it failed to run: ${installed_version}"
  say "Installed agent-archive ${installed_version} to ${target}"

  case ":${PATH}:" in
    *":${install_dir}:"*)
      say ""
      say "Next, run:  agent-archive setup"
      ;;
    *)
      say ""
      say "${install_dir} is not on your PATH. Add it with:"
      say "  echo 'export PATH=\"${install_dir}:\$PATH\"' >> ~/.zshrc && source ~/.zshrc"
      say "Then run:  agent-archive setup"
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    arm64 | aarch64) echo arm64 ;;
    x86_64)
      # A shell running under Rosetta reports x86_64 on Apple Silicon; the
      # native build is the right one there.
      if [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = "1" ]; then
        echo arm64
      else
        echo amd64
      fi
      ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
  esac
}

# verify_signature requires a valid, strict code signature from a Developer
# ID Application certificate that Apple issued to this project's team: the
# standard Developer ID designated requirement, with the team pinned.
verify_signature() {
  check_team_id
  requirement="anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \"${team_id}\""
  if ! codesign --verify --strict -R="$requirement" "$1" 2>/dev/null; then
    fail "${asset} is not signed by the agent-archive Developer ID (team ${team_id}); not installing it."
  fi
}

check_team_id() {
  if [ -z "$team_id" ]; then
    fail "this install script names no signing team yet, so there is no signed release to install. Build from source instead: https://github.com/wangjohn/agent-archive/blob/main/docs/getting-started/install.md"
  fi
  case "$team_id" in
    *[!A-Z0-9]*) fail "invalid signing team ID in this script: ${team_id}" ;;
  esac
}

choose_install_dir() {
  if [ -n "${AGENT_ARCHIVE_INSTALL_DIR:-}" ]; then
    echo "$AGENT_ARCHIVE_INSTALL_DIR"
    return
  fi
  # Upgrade in place, so the hooks and background collector setup recorded
  # keep pointing at the new binary.
  existing="$(command -v agent-archive 2>/dev/null || true)"
  if [ -n "$existing" ] && [ -w "$(dirname "$existing")" ]; then
    dirname "$existing"
    return
  fi
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    echo /usr/local/bin
    return
  fi
  echo "${HOME}/.local/bin"
}

download() {
  curl --fail --silent --show-error --location --proto '=https,file' --retry 3 \
    --output "$2" "$1" || fail "download failed: $1"
}

say() {
  printf '%s\n' "$1"
}

fail() {
  printf 'agent-archive install: %s\n' "$1" >&2
  exit 1
}

main "$@"
