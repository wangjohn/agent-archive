#!/bin/bash
# Actual disposable MinIO component acceptance, only on a fresh CI Linux runner.
# The service is source-built at the repository's previously exercised exact pin.
set -euo pipefail
umask 077
[[ ${GITHUB_ACTIONS:-} == true && $(uname -s) == Linux && $(id -u) != 0 ]] || {
  echo 'requires a disposable non-root Linux CI runner' >&2; exit 2;
}
: "${RUNNER_TEMP:?}" "${AA_ACCEPTANCE_OUTPUT:?}"
[[ $AA_ACCEPTANCE_OUTPUT == "$RUNNER_TEMP/acceptance-evidence" && ! -L $AA_ACCEPTANCE_OUTPUT ]] || { echo "evidence must stay in the private runner directory" >&2; exit 2; }
root=$(git rev-parse --show-toplevel)
[[ -z $(git status --porcelain) ]] || { echo 'acceptance requires a clean exact candidate' >&2; exit 2; }
[[ ! -e $RUNNER_TEMP/aa-provider-resources ]] || { echo "prior provider cleanup obligation exists" >&2; exit 2; }
suffix=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
prefix=aa-provider-$suffix
work=$RUNNER_TEMP/$prefix
mkdir -m 700 "$work"
container=$prefix-service
image=$prefix-image
printf '%s\n' "$prefix" > "$RUNNER_TEMP/aa-provider-resources"
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -d $AA_ACCEPTANCE_OUTPUT ]]; then
    docker logs "$container" > "$AA_ACCEPTANCE_OUTPUT/provider-service.txt" 2>&1 || true
    python3 "$root/scripts/acceptance/provider/sanitize-evidence.py" "$AA_ACCEPTANCE_OUTPUT" || status=1
  fi
  bash "$root/scripts/acceptance/provider/cleanup.sh" || status=1
  echo "provider cleanup finished (status=$status)"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$AA_ACCEPTANCE_OUTPUT" "$work/tools" "$work/data" "$work/mc" "$work/home"
export GOBIN="$work/tools" GOTOOLCHAIN=local
# SumDB checks the exact module versions; provenance and built module identities
# are recorded without static credentials, service data, or native transcript text.
for module in github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c github.com/minio/mc@v0.0.0-20251106162529-77f82e18b540; do
  CGO_ENABLED=0 go install "$module"
done
go version -m "$work/tools/minio" > "$AA_ACCEPTANCE_OUTPUT/minio-build.txt"
go version -m "$work/tools/mc" > "$AA_ACCEPTANCE_OUTPUT/mc-build.txt"
cp "$work/tools/minio" "$work/minio"
printf 'FROM scratch\nCOPY minio /minio\nENTRYPOINT ["/minio"]\n' > "$work/Dockerfile"
docker build --quiet --tag "$image" "$work" > "$AA_ACCEPTANCE_OUTPUT/provider-image.txt"
access=acceptance$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
secret=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
echo "::add-mask::$access"
echo "::add-mask::$secret"
export AA_PROVIDER_ACCESS="$access" AA_PROVIDER_SECRET="$secret"
printf 'MINIO_ROOT_USER=%s\nMINIO_ROOT_PASSWORD=%s\nHOME=/data\n' "$access" "$secret" > "$work/service.env"
docker run -d --name "$container" --user "$(id -u):$(id -g)" \
  --env-file "$work/service.env" --publish 127.0.0.1::9000 \
  --tmpfs /tmp:rw,mode=1777 --mount "type=bind,source=$work/data,target=/data" \
  "$image" server /data > /dev/null
port=$(docker port "$container" 9000/tcp)
[[ $port =~ ^127\.0\.0\.1:[0-9]+$ ]] || { echo 'provider is not confined to loopback' >&2; exit 1; }
endpoint=http://$port
ready=0
for _ in $(seq 1 60); do
  if curl --fail --silent --max-time 2 "$endpoint/minio/health/ready" >/dev/null; then ready=1; break; fi
  sleep 1
done
[[ $ready == 1 ]] || { echo 'real provider did not become ready' >&2; exit 1; }
"$work/tools/minio" --version > "$AA_ACCEPTANCE_OUTPUT/provider-version.txt"
"$work/tools/mc" --config-dir "$work/mc" alias set acceptance "$endpoint" "$access" "$secret" >/dev/null
"$work/tools/mc" --config-dir "$work/mc" mb acceptance/aa-disposable-acceptance >/dev/null
peer_access=peer$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
peer_secret=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
echo "::add-mask::$peer_access"
echo "::add-mask::$peer_secret"
export AA_PROVIDER_PEER_ACCESS="$peer_access" AA_PROVIDER_PEER_SECRET="$peer_secret"
cat > "$work/peer-policy.json" <<'POLICY'
{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucket","s3:GetBucketLocation"],"Resource":["arn:aws:s3:::aa-disposable-acceptance"]},{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::aa-disposable-acceptance/*"]}]}
POLICY
"$work/tools/mc" --config-dir "$work/mc" admin user add acceptance "$peer_access" "$peer_secret" >/dev/null
"$work/tools/mc" --config-dir "$work/mc" admin policy create acceptance acceptance-peer "$work/peer-policy.json" >/dev/null
"$work/tools/mc" --config-dir "$work/mc" admin policy attach acceptance acceptance-peer --user "$peer_access" >/dev/null
export AGENT_ARCHIVE_PROVIDER_ACCEPTANCE=1 AA_PROVIDER_ENDPOINT="$endpoint"
export AA_PROVIDER_ACCESS="$access" AA_PROVIDER_SECRET="$secret" AA_PROVIDER_BUCKET=aa-disposable-acceptance
# Go test source/native/state stays under temporary roots. The provider adapter
# also refuses ambient credentials, redirects, non-loopback origins and runaway requests.
cd "$root"
CGO_ENABLED=1 go test -json -race -p 2 -count=1 -timeout=8m -run '^TestProvider' ./internal/collector > "$AA_ACCEPTANCE_OUTPUT/provider-tests.jsonl"
python3 scripts/acceptance/provider/verify-results.py "$AA_ACCEPTANCE_OUTPUT/provider-tests.jsonl" \
  TestProviderAdmissionSurvivesNativeLossAndUncertainCommit \
  TestProviderFullSetPrivacyReadbackAndIndependentWinner \
  TestProviderIndependentOwnersPublishAndReadSeparateSessions \
  TestProviderImmutableMismatchNeverOverwritesSource \
  TestProviderPublishedSourceSurvivesNativeAndLocalStateLoss > "$AA_ACCEPTANCE_OUTPUT/provider-summary.json"
