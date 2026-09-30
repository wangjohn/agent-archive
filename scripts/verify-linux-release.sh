#!/usr/bin/env bash
# Checks the Linux binaries scripts/build-release.sh built, so a build error
# surfaces before publishing rather than when a user first runs `--version`.
# Used by the release workflow (on the tag's version) and by the Test workflow
# (on a placeholder version), so the release-only checks are exercised on
# every pull request.
#
# Usage: VERSION=v1.2.3 scripts/verify-linux-release.sh [dir]
#
#   - Each binary must be a statically linked ELF file (CGO_ENABLED=0), so it
#     runs on any distribution and libc, built for the architecture its name
#     says. Checked with readelf (a dynamically linked binary names an
#     interpreter), else `file`. With neither installed the check is skipped
#     with a notice, except when CI is set, where it fails.
#   - On an x86-64 Linux runner the amd64 binary runs natively and must report
#     VERSION. The arm64 binary is cross-compiled and cannot run there, so
#     its embedded version string is checked instead of executing it. On any
#     other machine (a Mac, an ARM machine) both binaries get the embedded
#     check.
set -euo pipefail

version="${VERSION:?set VERSION to the version the binaries were built with}"
dir="${1:-dist}"
amd64="$dir/agent-archive-linux-amd64"
arm64="$dir/agent-archive-linux-arm64"

fail() {
  echo "::error::$*" >&2
  exit 1
}

# check_binary FILE ARCH (amd64 or arm64). Returns 0 if FILE is a statically
# linked ELF binary for ARCH, 1 if it is not static (or not ELF at all), 3 if
# it is for another architecture (a swapped or mislabeled file), and 2 if this
# machine has no tool to tell. Process substitution rather than a pipe
# throughout: with pipefail, a grep that stops reading early would fail the
# pipeline.
check_binary() {
  local readelf_machine file_machine
  case "$2" in
    amd64) readelf_machine='X86-64' file_machine='x86-64' ;;
    arm64) readelf_machine='AArch64' file_machine='ARM aarch64' ;;
  esac
  if command -v readelf >/dev/null 2>&1; then
    # Not an ELF file at all (a Mach-O binary, a script) prints nothing on
    # stdout, which the INTERP search below would read as "static".
    readelf -h "$1" >/dev/null 2>&1 || return 1
    grep -q "Machine:.*${readelf_machine}" < <(readelf -h "$1") || return 3
    grep -q INTERP < <(readelf -lW "$1") && return 1
    return 0
  fi
  if command -v file >/dev/null 2>&1; then
    grep -q "ELF.*${file_machine}" < <(file "$1") || return 3
    grep -q 'ELF.*statically linked' < <(file "$1") && return 0
    return 1
  fi
  return 2
}

for arch in amd64 arm64; do
  binary="$dir/agent-archive-linux-${arch}"
  [ -f "$binary" ] || fail "$binary is missing"
  status=0
  check_binary "$binary" "$arch" || status=$?
  case "$status" in
    0) ;;
    1) fail "$binary is not a statically linked ELF binary (was it built with CGO_ENABLED=0?)" ;;
    3) fail "$binary is not a linux/${arch} binary" ;;
    *)
      # Release CI must not pass unchecked; a local run may.
      if [ -n "${CI:-}" ]; then
        fail "neither readelf nor file is installed, so $binary cannot be checked"
      fi
      echo "notice: neither readelf nor file is installed; skipping the static-linking and architecture checks for $binary" >&2
      ;;
  esac
done

embedded=("$arm64")
if [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ]; then
  got="$("$amd64" --version 2>&1 || true)"
  if [ "$got" != "$version" ]; then
    fail "agent-archive --version (linux/amd64) reported '$got', expected '$version'"
  fi
else
  embedded=("$amd64" "$arm64")
fi

for binary in "${embedded[@]}"; do
  grep -qxF "$version" < <(strings "$binary") || fail "$binary does not embed version '$version'"
done

echo "linux binaries OK (version $version)"
