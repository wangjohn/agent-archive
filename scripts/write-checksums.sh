#!/usr/bin/env bash
# Writes SHA256SUMS for the release binaries in a directory (default dist/).
# The one place the released binaries are named for checksumming, shared by
# scripts/build-release.sh and the release workflow's post-signing step, so
# the checksum file always lists all four, in this order.
#
# The file format is what `sha256sum -c`, `shasum -a 256 -c`, and install.sh
# all read: "<hex digest>", two spaces, the bare file name. It uses
# sha256sum when present (Linux) and shasum -a 256 otherwise (macOS), which
# print the same format.
#
# Usage: scripts/write-checksums.sh [dir]
set -euo pipefail

dir="${1:-dist}"
assets=(
  agent-archive-darwin-amd64
  agent-archive-darwin-arm64
  agent-archive-linux-amd64
  agent-archive-linux-arm64
)

if command -v sha256sum >/dev/null 2>&1; then
  sum=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  sum=(shasum -a 256)
else
  echo "need sha256sum or shasum to write SHA256SUMS" >&2
  exit 1
fi

cd "$dir"
for asset in "${assets[@]}"; do
  if [ ! -f "$asset" ]; then
    echo "cannot write SHA256SUMS: $dir/$asset is missing" >&2
    exit 1
  fi
done
"${sum[@]}" "${assets[@]}" > SHA256SUMS
