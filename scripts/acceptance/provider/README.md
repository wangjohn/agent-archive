# Exact-candidate provider and native runtime acceptance

`Provider and native runtime acceptance` runs on pull requests whose head branch
starts with `codex/backfill-`, when lifecycle or acceptance sources change. A
maintainer may also use workflow dispatch against a selected ref. Each job checks
out and records the **exact head commit**, rather than GitHub's synthetic merge
commit. Required ordinary PR checks remain unchanged and still test integration.
A changed candidate invalidates previous acceptance; rerun the relevant jobs and
record all exact dependency/head identities before readiness.

The native jobs execute the real SQLite held-file, durable admission, collector,
privacy replay and generation components on four separately recorded platforms:
Ubuntu24.04 amd64/arm64, Darwin15 Intel amd64 and Darwin14 arm64. They verify
native GOOS/GOARCH/GOHOSTOS/GOHOSTARCH and non-root execution, and record actual
kernel/OS, Go and Git versions. Labels follow the [official runner
reference](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
This is execution, separate from the existing architecture release cross-builds.
Synthetic producer fixtures do not establish actual Codex/Cursor/app-version
support. The native asset wrapper executes both pinned Darwin amd64 releases v0.1.0
and v0.1.1 through the inherited config-refusal and history-command engines.
Darwin arm64 executes pinned v0.1.1 config-refusal checks; a v0.1.0 arm64
asset is not pinned or claimed. Linux has no published old assets: any source-tag
build must be labeled separately. These synthetic endpoint checks are separate
from genuine MinIO acceptance.

The Linux amd64 provider job executes `bash scripts/acceptance/provider/host.sh`
inside a fresh disposable non-root Actions VM. It requires Docker already supplied
by CI, installs no local daemon, and refuses a developer's host. It source-builds
MinIO and mc from the exact versions previously exercised by the repository's
Linux live acceptance run:

- MinIO `v0.0.0-20260212201848-7aac2a2c5b7c`, [source
  commit](https://github.com/minio/minio/commit/7aac2a2c5b7c882e68c1ce017d8256be2feea27f).
- mc `v0.0.0-20251106162529-77f82e18b540`, [source
  commit](https://github.com/minio/mc/commit/77f82e18b5401a65958f1619df6ebb994634bd88).

Go1.27.1 verifies those module versions through SumDB and records each binary's
module/build identity and actual service version. The provider image is built
from scratch, avoiding previously unavailable unpinned registry images. Source
or image failure is a failed gate, never a fallback to a fake service.

A loopback-only ephemeral MinIO serves one private disposable bucket. Credentials
are generated only for this run; a second issued static identity has object and
listing access to that bucket. Production S3 clients get explicit credentials and
cannot consult the user's AWS profile or Keychain. Every test receives a random
prefix, and cleanup inventories/deletes only that prefix. HTTP attempts/statuses
are counted with a4096-attempt cap per fixture, a15-second request timeout and
bounded source verification. Cancellation must perform no provider work. The
single-source verification check has a separate operation budget. The two owners
have separate local state/native fixtures and credential/client identities;
shared-destination conflict detection is still read/compare/write under the
existing single-owner contract, **not distributed CAS** or cloned-owner safety.

Executed provider tests cover uncertain metadata acknowledgement **after an actual
successful provider PUT**, journal restart/native loss before upload, retained production reading after
native and local-state loss following upload, full current
and preserved source checksum/age/content verification, actual native privacy
refilter, changed authoritative winner/listing publication, retained reading
without local state, immutable mismatch refusal, and two independent owners'
staged publication/readback. Fault injection reports an acknowledgement loss
after a real S3 response; the underlying provider is never replaced. Deletion,
retention/undo/purge/restoration and old-release checks must be integrated from the
owning deletion PR before final P7a acceptance. Until then runs are provisional.

The service step has a25-minute timeout inside a35-minute job. A private owned
resource control names only this run's container/image/root. EXIT/signal cleanup
and a separate always-run cleanup step verify absence; unavailable/uncertain
cleanup retains the control and fails. Whole disposable VM teardown is the final
backstop on job cancellation. Artifacts contain candidate/platform/provenance,
JSON test events and summaries plus sanitized service diagnostics. Ephemeral
credential values are masked in logs, removed from artifacts and make acceptance
fail if they unexpectedly appear. No provider data or credential files are
uploaded. `verify-results.py` requires every named test and package to pass;
missing, skipped, failed, queued, canceled or timed-out work cannot count.

Local harness policy tests are safe and require no Docker or provider:

```sh
python3 scripts/test_provider_acceptance.py
```

They check owned cleanup success/failure/unknown evidence and invalid controls,
credential redaction, exact candidate/runner policy and rejection of absent or
skipped tests. They are infrastructure checks, not provider acceptance. Provider
fixtures compile and skip unless explicitly opted in; a skip is not a live pass.

All shipped history mutation fences stay in place. Current full-set storage and
native maintenance components are exercised through their actual ports. A fenced
entrypoint refusal does not prove successful history lifecycle execution. The
prospective P4c enabled candidate must extend/run this harness through newly
reachable complete lifecycle paths and pass exact-candidate provider/runtime
acceptance before readiness. MinIO establishes genuine disposable S3-compatible
behavior; it does not establish AWS/R2 account permissions, live app layouts,
network homes, or other provider-specific guarantees. Record those gaps where the
task or supported-provider correctness contract requires them.

The provider suite also exercises purge preservation at the complete 65-reference
bound, corrupt/incomplete/unreadable selecting metadata refusal, uncertain
metadata-first owned deletion after native loss, service-clock retention expiry,
new-hook exact restoration and missing/changed restoration-intent refusal.
Full-history deletion uses the production inner journal/verification/namespace
ports while explicitly asserting the public history fence. Privacy retirement
uses actual CLI verification evidence and the production full-selection cleanup
guard; every retained revision is read back with content and capture-age checks.
Successful outer historical lifecycle commands remain prospective P4c acceptance.
