# Listing performance target

The baseline implementation lists every object below `sessions/` and reads
every metadata sidecar before applying the default `--limit 50`. A fixture
with 10,000 sessions and three source snapshots per session therefore
requires 40,000 listed keys and 10,000 sidecar reads on a cold cache. A warm
cache avoids downloads but still lists all 40,000 keys and reads 10,000 local
sidecars.

For an indexed archive with 10,000 sessions, the target for an unfiltered
default list is at most **two listing pages** and **60 metadata reads** on
both cold and warm runs, with no source downloads. A full scan remains
available with `--limit 0`, and an older archive without an index uses that
scan until `list --rebuild-index` completes. The index is a hint: every
displayed session must pass a live sidecar read and validation.

The benchmark in `internal/reader/list_index_bench_test.go` reports elapsed
time and allocations for the cold full-scan baseline, indexed cold listing,
and indexed warm listing. Memory-store listing is an in-process stand-in for
remote pages; the request-count target above is the release gate.
