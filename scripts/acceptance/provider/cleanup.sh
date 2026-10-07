#!/bin/bash
# Retry only resources named in this run's private control; never discover peers.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && $(uname -s) == Linux && $(id -u) != 0 ]] || exit 2
: "${RUNNER_TEMP:?}"
control=$RUNNER_TEMP/aa-provider-resources
[[ -e $control ]] || exit 0
[[ -f $control && ! -L $control ]] || { echo 'invalid provider cleanup control' >&2; exit 1; }
IFS= read -r prefix < "$control"
[[ $prefix =~ ^aa-provider-aa-provider\.[A-Za-z0-9]{8}$ ]] || { echo 'invalid owned provider resource name' >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "cannot verify provider cleanup while daemon is unavailable" >&2; exit 1; }
inspect_owned() {
  local kind=$1 name=$2 error
  if error=$(docker "$kind" inspect "$name" 2>&1 >/dev/null); then return 0; fi
  case $error in
    "Error: No such object: $name" | "Error: No such image: $name" | "Error response from daemon: No such container: $name" | "Error response from daemon: No such image: $name") return 1 ;;
    *) echo 'owned resource absence could not be verified' >&2; return 2 ;;
  esac
}
status=0
for kind in container image; do
  suffix=service
  [[ $kind == container ]] || suffix=image
  name=$prefix-$suffix
  result=0
  inspect_owned "$kind" "$name" || result=$?
  if [[ $result == 0 ]]; then
    if [[ $kind == container ]]; then docker rm -f "$name" >/dev/null || status=1
    else docker image rm "$name" >/dev/null || status=1; fi
    result=0
    inspect_owned "$kind" "$name" || result=$?
    [[ $result == 1 ]] || status=1
  elif [[ $result != 1 ]]; then status=1
  fi
done
docker info >/dev/null 2>&1 || status=1
if [[ $status == 0 ]]; then
  rm -rf "$RUNNER_TEMP/${prefix#aa-provider-}"
  rm "$control"
else
  echo 'owned provider cleanup remains incomplete' >&2
fi
exit "$status"
