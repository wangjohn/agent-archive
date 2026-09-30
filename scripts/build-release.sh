#!/usr/bin/env bash
# Builds agent-archive's release binaries and their SHA-256 checksums into
# dist/: macOS on Intel and ARM64 (agent-archive-darwin-{amd64,arm64}) and
# Linux on x86-64 and ARM64 (agent-archive-linux-{amd64,arm64}). Used by both
# the release workflow and local maintainers, so the two never drift:
# whatever this script produces is exactly what gets signed (macOS only),
# notarized (macOS only), and published.
#
# The macOS binaries need cgo (the Keychain), so they build only on macOS with
# Xcode's clang. The Linux binaries are pure Go (CGO_ENABLED=0, static, using
# the credential stub) and cross-compile from any machine. Neither Linux
# binary is signed; trust in them rests on SHA256SUMS and the build
# provenance attestation (dev/maintainers/releasing.md).
#
# Usage: VERSION=v1.2.3 scripts/build-release.sh [darwin] [linux]
# With no arguments both platforms are built, which is what a release and a
# local full build want. Naming one builds only that platform, for a runner
# that cannot build the other (the release workflow builds Linux on Linux).
# SHA256SUMS lists all four binaries, so it is written only when both
# platforms were built; the publish job recomputes it after signing anyway.
# VERSION defaults to "dev" (a build that reports `agent-archive --version`
# as "dev-<commit>", never suitable for release).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

version="${VERSION:-dev}"
out_dir="dist"

platforms=("$@")
if [ "${#platforms[@]}" -eq 0 ]; then
  platforms=(darwin linux)
fi
build_darwin=false
build_linux=false
for platform in "${platforms[@]}"; do
  case "$platform" in
    darwin) build_darwin=true ;;
    linux) build_linux=true ;;
    *)
      echo "unknown platform '$platform'; use darwin, linux, or no arguments for both" >&2
      exit 2
      ;;
  esac
done

rm -rf "$out_dir"
mkdir -p "$out_dir"

ldflags="-s -w -X github.com/wangjohn/agent-archive/internal/cli.Version=${version}"

if $build_darwin; then
  for arch in amd64 arm64; do
    binary="$out_dir/agent-archive-darwin-${arch}"
    echo "building $binary (version=${version})"
    GOOS=darwin GOARCH="$arch" CGO_ENABLED=1 go build -trimpath -ldflags "$ldflags" -o "$binary" ./cmd/agent-archive
  done
fi

if $build_linux; then
  for arch in amd64 arm64; do
    binary="$out_dir/agent-archive-linux-${arch}"
    echo "building $binary (version=${version})"
    GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$binary" ./cmd/agent-archive
  done
fi

if $build_darwin && $build_linux; then
  scripts/write-checksums.sh "$out_dir"
else
  echo "skipping SHA256SUMS: it lists every platform's binaries and only a full build has them"
fi

echo "built:"
ls -la "$out_dir"
