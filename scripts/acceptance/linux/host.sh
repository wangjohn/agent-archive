#!/bin/bash
# Live acceptance of agent-archive on Linux with systemd: the real setup, hooks,
# timer, collector, status, refresh and uninstall, in a disposable privileged
# Ubuntu 24.04 container that runs systemd as PID 1, with a throwaway MinIO as
# the bucket. See README.md in this directory. Run it from anywhere inside a
# checkout, on a machine with Docker (Docker Desktop, colima):
#
#   scripts/acceptance/linux/host.sh            # the run, about 3 minutes warm
#   scripts/acceptance/linux/host.sh --dry-run  # say what it would do; needs no Docker
#
# It cross-builds linux binaries of this checkout, starts MinIO and the machine
# on a network of their own, runs guest.sh in the machine as root, prints every
# check as PASS or FAIL, and removes the containers, network, volume and image
# it made when it ends, whether it passed, failed or was interrupted (KEEP=1
# leaves them for a look). Everything it creates is named aa-accept-<random>-*;
# it never lists, stops or removes anything else (a MinIO container of your own,
# say). The exit status is 0 only if every check passed. Only synthetic content
# is used (the repository's fixture transcript and a hand-made Cursor database)
# and the only credentials are a MinIO user and password made up for the run.
# Nothing Linux runs on the machine this is run from: it is only Docker's client
# and, to cross-build, Go.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
minio_image=${MINIO_IMAGE:-quay.io/minio/minio}
mc_image=${MC_IMAGE:-quay.io/minio/mc}
dry_run=0

usage() {
  sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
  echo
  echo "Environment: KEEP=1 keeps the run's containers; MINIO_IMAGE and MC_IMAGE name other images;"
  echo "BUILD_IN_DOCKER=1 builds in a golang container, so the machine needs no Go."
}
for arg in "$@"; do
  case $arg in
    --dry-run) dry_run=1 ;;
    -h | --help) usage; exit 0 ;;
    *) echo "unknown argument $arg" >&2; usage >&2; exit 2 ;;
  esac
done

# One name prefix for everything this run makes. Cleanup refuses any name that
# does not start with it, so a bug here cannot reach a container of yours.
suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
prefix=aa-accept-$suffix
host=$prefix-machine
minio=$prefix-minio
net=$prefix-net
image=$prefix-image
volume=$prefix-build
builder=$prefix-builder
access=acc$suffix
secret=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
bucket=acceptance-archive
work=
started=$(date +%s)

# own NAME: succeeds only for a name this run made.
# shellcheck disable=SC2329  # called from cleanup, which only a trap calls
own() { case $1 in "$prefix"-*) return 0 ;; *) echo "refusing to touch $1: not made by this run ($prefix-*)" >&2; return 1 ;; esac }

if [ "$dry_run" = 1 ]; then
  cat <<EOF
dry run: nothing is built, pulled or started.
Docker would be asked to make, and to remove when the run ends:
  network    $net
  container  $minio ($minio_image, no published ports)
  container  $builder (golang, only when BUILD_IN_DOCKER=1)
  container  $host (ubuntu:24.04 with systemd as PID 1, privileged, from image $image)
  volume     $volume (only when BUILD_IN_DOCKER=1)
and to run $mc_image once to make the bucket $bucket, and once more to list it.
It would build ./cmd/agent-archive and the cli, systemd and cursorstore test binaries for
linux/<Docker's architecture> from $root, copy them with guest.sh and the fixture
internal/archive/testdata/claude-model-tokens.jsonl into $host:/acceptance, and run guest.sh there.
Nothing else is touched.
EOF
  exit 0
fi

# shellcheck disable=SC2329  # run by the EXIT trap
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [ "${KEEP:-}" = 1 ]; then
    echo "kept: containers $host and $minio, network $net, image $image, volume $volume"
    echo "  remove them with: docker rm -f $host $minio; docker network rm $net; docker rmi $image; docker volume rm $volume"
  else
    for c in "$host" "$minio" "$builder"; do own "$c" && docker rm -f "$c" >/dev/null 2>&1 || true; done
    own "$net" && docker network rm "$net" >/dev/null 2>&1 || true
    own "$image" && docker rmi "$image" >/dev/null 2>&1 || true
    own "$volume" && docker volume rm "$volume" >/dev/null 2>&1 || true
  fi
  [ -z "$work" ] || rm -rf "$work"
  echo "(took $(($(date +%s) - started))s)"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

command -v docker >/dev/null || { echo "docker is not installed or not on PATH" >&2; exit 2; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon cannot be reached (is Docker Desktop or colima running?)" >&2; exit 2; }
work=$(mktemp -d)

arch=$(docker info --format '{{.Architecture}}')
case $arch in
  aarch64 | arm64) goarch=arm64 ;;
  x86_64 | amd64) goarch=amd64 ;;
  *) echo "unsupported Docker architecture $arch" >&2; exit 2 ;;
esac

echo "== building linux/$goarch binaries from $(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo "this tree") (and its working tree)"
if [ "${BUILD_IN_DOCKER:-}" = 1 ] || ! command -v go >/dev/null; then
  # In a golang container (this checkout mounted read-only; the module and build
  # caches in a volume of the run's own), and the results copied out with
  # docker cp, so nothing needs to be shared with the Docker VM but the checkout.
  goversion=$(sed -n 's/^toolchain go//p' "$root/go.mod")
  docker run --name "$builder" -v "$root":/src:ro -v "$volume":/cache -w /src \
    -e GOOS=linux -e GOARCH="$goarch" -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
    -e GOCACHE=/cache/go-build -e GOMODCACHE=/cache/mod -e GOTOOLCHAIN=local \
    "golang:$goversion" sh -ec '
      mkdir /out
      go build -o /out/agent-archive ./cmd/agent-archive
      go test -c -o /out/cli.test ./internal/cli
      go test -c -o /out/systemd.test ./internal/scheduler/systemd
      go test -c -o /out/cursorstore.test ./internal/cursorstore'
  for f in agent-archive cli.test systemd.test cursorstore.test; do docker cp "$builder:/out/$f" "$work/$f"; done
  docker rm -f "$builder" >/dev/null
else
  build() { (cd "$root" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go "$@"); }
  build build -o "$work/agent-archive" ./cmd/agent-archive
  # The tests that drive the real commands over the real user manager.
  build test -c -o "$work/cli.test" ./internal/cli
  build test -c -o "$work/systemd.test" ./internal/scheduler/systemd
  build test -c -o "$work/cursorstore.test" ./internal/cursorstore
fi
cp "$root/internal/archive/testdata/claude-model-tokens.jsonl" "$work/fixture-claude.jsonl"

echo "== starting MinIO and the machine"
docker network create "$net" >/dev/null
docker run -d --name "$minio" --network "$net" \
  -e MINIO_ROOT_USER="$access" -e MINIO_ROOT_PASSWORD="$secret" \
  "$minio_image" server /data >/dev/null
docker build -q -t "$image" "$here" >/dev/null
docker run -d --name "$host" --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
  --network "$net" "$image" /sbin/init >/dev/null

state=
for _ in $(seq 1 90); do
  state=$(docker exec "$host" systemctl is-system-running 2>/dev/null || true)
  # A container with no failed boot unit says "running"; one whose boot left a
  # unit failed (a service that needs hardware) says "degraded", which is as
  # up as the checks need.
  case $state in running | degraded) break ;; esac
  sleep 1
done
echo "systemd is $state"
case $state in running | degraded) ;; *) echo "the machine's systemd did not come up" >&2; exit 1 ;; esac

# The bucket, made with MinIO's client from a container on the same network.
docker run --rm --network "$net" --entrypoint /bin/sh "$mc_image" -c \
  "for i in \$(seq 1 90); do mc alias set local http://$minio:9000 $access $secret >/dev/null 2>&1 && break; sleep 1; done; mc mb --ignore-existing local/$bucket" ||
  { echo "could not make the bucket; MinIO says:" >&2; docker logs --tail 20 "$minio" >&2; exit 1; }

docker exec "$host" mkdir -p /acceptance
for f in agent-archive cli.test systemd.test cursorstore.test fixture-claude.jsonl; do
  docker cp "$work/$f" "$host:/acceptance/$f"
done
docker cp "$root/internal/scheduler/systemd/testdata" "$host:/acceptance/systemd-testdata"
docker cp "$here/guest.sh" "$host:/acceptance/guest.sh"

echo "== running the acceptance checks in the machine ($goarch)"
status=0
# In the background and waited for, so that a signal to this script runs the
# cleanup at once (bash holds a trap back until a foreground command ends).
docker exec \
  -e AA_ACCEPT_GUEST="$prefix" \
  -e ENDPOINT="http://$minio:9000" -e ACCESS_KEY="$access" -e SECRET_KEY="$secret" -e BUCKET="$bucket" \
  "$host" bash /acceptance/guest.sh &
wait $! || status=$?

echo "== what the bucket holds (from outside the machine)"
docker run --rm --network "$net" --entrypoint /bin/sh "$mc_image" -c \
  "mc alias set local http://$minio:9000 $access $secret >/dev/null && mc ls --recursive local/$bucket | head -30" || true
machine=$(docker exec "$host" sh -c '. /etc/os-release && echo "$PRETTY_NAME, $(systemctl --version | head -1)"' 2>/dev/null || echo "unknown (the machine is gone)")
echo "== the machine: linux/$goarch, $machine"
exit "$status"
