#!/bin/bash
# Executes existing compatibility engines with exact native published assets.
set -euo pipefail
umask 077
[[ ${GITHUB_ACTIONS:-} == true && $(uname -s) == Darwin && $(id -u) != 0 ]] || exit 2
: "${RUNNER_TEMP:?}" "${AA_ACCEPTANCE_OUTPUT:?}"
work=$(mktemp -d "$RUNNER_TEMP/aa-published.XXXXXXXX")
trap 'rm -rf "$work"' EXIT
case $(uname -m) in
  x86_64) arch=amd64; versions='v0.1.0 v0.1.1' ;;
  arm64) arch=arm64; versions='v0.1.1' ;;
  *) exit 2 ;;
esac
for version in $versions; do
  case "$version/$arch" in
    v0.1.0/amd64) sha=43fbe6d8d65d2d32ebad66d55d116e10d517c40908032297bd5977beb45ab5ad ;;
    v0.1.1/amd64) sha=c8fff68b623a7e0143503adce2d83c6494d7efa55fa0724eb4f76ae54c66255e ;;
    v0.1.1/arm64) sha=09f08430627cc0c867afd41ce7eb2c3e61859b640dfb2994323577847eee06e6 ;;
    *) exit 2 ;;
  esac
  mkdir "$work/$version"
  binary="$work/$version/agent-archive-darwin-$arch"
  curl --fail --location --silent --show-error --max-time 120 \
    "https://github.com/wangjohn/agent-archive/releases/download/$version/agent-archive-darwin-$arch" --output "$binary"
  printf '%s  %s\n' "$sha" "$binary" | shasum -a 256 --check
  chmod 700 "$binary"
  AGENT_ARCHIVE_OLD_BINARY="$binary" AGENT_ARCHIVE_OLD_RELEASE="$version" go test -json -count=1 -timeout=3m \
    -run '^TestPublishedWriterRefuses(ProtectedConfiguration|HistoryAndStagedConfiguration)$' ./internal/config \
    > "$AA_ACCEPTANCE_OUTPUT/published-$version-$arch.jsonl"
  python3 scripts/acceptance/provider/verify-results.py "$AA_ACCEPTANCE_OUTPUT/published-$version-$arch.jsonl" \
    TestPublishedWriterRefusesProtectedConfiguration TestPublishedWriterRefusesHistoryAndStagedConfiguration
 done
if [[ $arch == amd64 ]]; then
  python3 scripts/test_history_published_compatibility.py --artifacts "$work"
fi
