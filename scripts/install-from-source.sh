#!/usr/bin/env bash
# Build this checkout for the current Mac and install a persistent dev binary.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/install-from-source.sh [--replace-current | --destination PATH]

Build the current source tree, including uncommitted changes. By default,
install to ~/.local/share/agent-archive-dev/bin/agent-archive without changing
the release installation. --replace-current replaces the agent-archive found
on PATH. --destination installs to an explicit absolute executable path.
The script does not run setup or change archive data, hooks, or launchd.
EOF
}

fail() {
  printf 'install-from-source: %s\n' "$1" >&2
  exit 1
}

mode=isolated
destination=
while (($#)); do
  case "$1" in
    --replace-current)
      [[ "$mode" == isolated ]] || fail 'choose one installation destination'
      mode=current
      ;;
    --destination)
      [[ "$mode" == isolated ]] || fail 'choose one installation destination'
      (($# >= 2)) || fail '--destination needs an absolute path'
      mode=explicit
      destination=$2
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *) fail "unknown option: $1" ;;
  esac
  shift
done

[[ "$(uname -s)" == Darwin ]] || fail 'macOS is required'
command -v go >/dev/null 2>&1 || fail 'Go is required (see go.mod for the toolchain version)'

case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64)
    if [[ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" == 1 ]]; then
      arch=arm64
    else
      arch=amd64
    fi
    ;;
  *) fail 'unsupported Mac architecture' ;;
esac

case "$mode" in
  isolated) destination="$HOME/.local/share/agent-archive-dev/bin/agent-archive" ;;
  current)
    destination="$(type -P agent-archive || true)"
    [[ -n "$destination" ]] || fail 'no agent-archive executable found on PATH; use --destination PATH'
    ;;
  explicit)
    [[ "$destination" == /* ]] || fail '--destination must be an absolute path'
    ;;
esac
[[ "$(basename "$destination")" == agent-archive ]] || fail 'destination filename must be agent-archive'
[[ ! -L "$destination" ]] || fail "destination is a symlink: $destination"
[[ ! -e "$destination" || -f "$destination" ]] || fail "destination is not a regular file: $destination"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
install_dir="$(dirname "$destination")"
mkdir -p "$install_dir"
[[ -w "$install_dir" ]] || fail "destination directory is not writable: $install_dir"

build_dir="$(mktemp -d)"
staged=
cleanup() {
  rm -rf "$build_dir"
  if [[ -n "$staged" ]]; then rm -f "$staged"; fi
}
trap cleanup EXIT

binary="$build_dir/agent-archive"
printf 'Building %s for darwin/%s...\n' "$repo_root" "$arch"
(
  cd "$repo_root"
  GOOS=darwin GOARCH="$arch" CGO_ENABLED=1 go build -trimpath \
    -ldflags '-s -w -X github.com/wangjohn/agent-archive/internal/cli.Version=dev' \
    -o "$binary" ./cmd/agent-archive
)
version="$("$binary" --version)" || fail 'the new binary failed its version check'
[[ "$version" == dev-* ]] || fail "expected a dev version, got: $version"

# Rename a fully built file over the destination. Copying onto a running
# Mach-O executable in place can make macOS kill it at launch.
staged="$(mktemp "$install_dir/.agent-archive.install.XXXXXXXX")"
cp "$binary" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$destination"
staged=

printf 'Installed %s at %s\n' "$version" "$destination"
printf 'Try: "%s" status\n' "$destination"
