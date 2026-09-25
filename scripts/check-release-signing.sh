#!/usr/bin/env bash
# Validate presence without printing credential values. Used only by release CI.
set -euo pipefail
if [[ "${APPLE_SIGNING_ENABLED:-}" != true ]]; then
  echo 'Release signing must be enabled; unsigned releases are not published.' >&2
  exit 1
fi
for name in APPLE_CERTIFICATE_P12_BASE64 APPLE_CERTIFICATE_PASSWORD APPLE_SIGNING_IDENTITY APPLE_ID APPLE_TEAM_ID APPLE_APP_SPECIFIC_PASSWORD; do
  if [[ -z "${!name:-}" ]]; then
    echo "Missing release secret: $name" >&2
    exit 1
  fi
done
# install.sh verifies a downloaded binary against the team ID it names, so a
# release signed by any other team would install for nobody. Refuse to
# publish one, without printing either value.
install_sh="${INSTALL_SH:-$(dirname "${BASH_SOURCE[0]}")/../install.sh}"
pinned="$(sed -n 's/^team_id="\([A-Z0-9]*\)"$/\1/p' "$install_sh")"
if [[ "$(grep -c '^team_id=' "$install_sh")" != 1 ]]; then
  echo 'install.sh must set team_id exactly once, as team_id="<TEAM ID>".' >&2
  exit 1
fi
if [[ -z "$pinned" || "$pinned" != "$APPLE_TEAM_ID" ]]; then
  echo 'install.sh team_id must equal the APPLE_TEAM_ID secret (set it before the first release).' >&2
  exit 1
fi
