# Linux live acceptance

A real `setup`, hooks, timer, collector, `status`, `setup --refresh` and
`uninstall` on Linux, against a real systemd user manager, in a disposable
container. It is the live run the platform proposal's step 5c asks for;
[testing](../../../dev/contributing/testing.md#the-linux-live-acceptance-run)
says what it covers and records the last result.

```sh
scripts/acceptance/linux/host.sh            # the run: about 3 minutes with Docker's caches warm, mostly waiting on systemd timers
scripts/acceptance/linux/host.sh --dry-run  # prints what it would create; needs no Docker
```

It needs Docker (Docker Desktop, colima, or a Linux Docker) and either Go (to
cross-build the binary and test binaries) or `BUILD_IN_DOCKER=1`, which builds in
a `golang` container instead. It is not CI: the machine is a privileged
container. `python3 scripts/test_linux_acceptance.py` (which CI does run) checks
the scripts without Docker: shellcheck, `--dry-run`, and, with a fake `docker`,
that cleanup removes only what the run made.

## What it does

`host.sh` runs on your machine and only drives Docker:

1. builds `linux/<Docker's architecture>` binaries of this checkout (the working
   tree, uncommitted changes included): the CLI and the `cli`, `scheduler/systemd`
   and `cursorstore` test binaries;
2. starts a MinIO (`quay.io/minio/minio`, a user and password made up for the run,
   no published ports) and a machine (`Dockerfile`: Ubuntu 24.04 with systemd as
   PID 1, privileged, two lingering users, `ada` and `bob`) on a network of their
   own, and makes a bucket;
3. copies the binaries, the fixture
   `internal/archive/testdata/claude-model-tokens.jsonl` and `guest.sh` into the
   machine and runs `guest.sh` there as root;
4. prints every check as `PASS` or `FAIL`, lists what the bucket holds, and
   removes what it made.

`guest.sh` refuses to run anywhere else. It is where everything Linux happens.
Its sections:

| Section | What it checks |
| --- | --- |
| 0 | the machine: systemd's version, `ada`'s user manager and bus, a machine ID |
| 1-2 | `setup --yes` (S3 to MinIO) from a shell with `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` set: the unit files (mode, `Environment=`, `PATH`, `append:` logging), the enable link, the timer waiting, `config.json` (`background_backend`, a `host_id` that is not the machine ID), status says loaded |
| 3 | a synthetic Claude Code session through the installed hook command (the fixture transcript; `SessionStart`, `UserPromptSubmit`, `Stop`, `SessionEnd`), published by the **timer's** collector run, not by `sync`; read back from the bucket with `list` and `show`; the journal shows the timer started the service |
| 4 | a hand-made Cursor database, imported by `backfill`; its copy goes under `$XDG_CACHE_HOME/agent-archive/cursor-snapshots` (0700, `CACHEDIR.TAG`, removed after the read), never `/tmp` or the default cache |
| 5 | `status` warns when the shell's XDG directories differ from the collector's, and is quiet when they match |
| 6 | the machine-ID clone warning: quiet where it was set up; after `/etc/machine-id` changes, `status` and `status --json` warn and name the fix, `setup` warns and keeps the recorded `host_id`; a machine ID bind-mounted over `/etc/machine-id` is not read, so it stays quiet; restoring the ID silences it |
| 7 | `setup --refresh` after the binary moved: the unit and hooks run the new path, the recorded environment is kept, the timer keeps firing |
| 8 | `uninstall`: the unit files, the enable link, the manager's listing and the hooks are gone |
| 9 | the enable-link cleanup when a setup's start fails after `systemctl enable` made the link (a `systemctl` wrapper in `ada`'s `PATH` enables without starting, then fails): setup fails, the rollback leaves no files and no link (not even a dangling one) and no running timer; the next setup works |
| 10 | `uninstall --skip-scheduler` from a session that reaches the manager: stops the job, removes the files and the link |
| 11 | `uninstall` from a session with no user bus refuses and names the flag and the manual stop command; with `--skip-scheduler` it says the job was not verified stopped, removes the files and leaves **no dangling enable link**; the manager still runs the timer until the printed stop command is run, which stops it |
| 12 | the real-manager Go tests (`TestRealSystemd...`, the adapter's conformance run over the real manager) as `bob`, a second lingering user |
| 13 | `cursorstore`'s snapshot tests on a real Linux account (the account's home, not `$HOME`) |

The enable-link cleanup is therefore checked on every path that removes a job:
uninstall with a manager (8), `--skip-scheduler` with a manager (10),
`--skip-scheduler` without one (11), and a setup rollback (9).

## Safety

- Everything it makes is named `aa-accept-<random>-*` (the network, the two
  containers, the image, and a build volume when `BUILD_IN_DOCKER=1`). Cleanup
  (an `EXIT`, `INT` and `TERM` trap) removes names with that prefix only, and
  refuses any other. It never lists, stops or removes another container,
  including a MinIO of your own such as `agent-archive-minio`; MinIO publishes no
  port, so it cannot collide with one.
- `KEEP=1` keeps the run's containers, network and image for a look, and prints
  the commands that remove them.
- Pulled images (`quay.io/minio/minio`, `quay.io/minio/mc`, `ubuntu:24.04`,
  `golang:<toolchain>` for `BUILD_IN_DOCKER=1`) stay in Docker's cache. Set
  `MINIO_IMAGE` and `MC_IMAGE` to use others.
- The only credentials are a MinIO user and password generated per run, passed
  to the container and gone with it. Content is synthetic: the repository's
  fixture transcript and a Cursor database the script writes. Nothing of yours
  reaches the container, and nothing Linux runs on your machine.

## When it fails

A failed check prints the output it looked at. `KEEP=1` leaves the machine up:
`docker exec -it aa-accept-<random>-machine bash`, then
`sudo -u ada -H env XDG_RUNTIME_DIR=/run/user/1100 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1100/bus bash`
to be `ada` with her manager. A failure in a section after the first is often
the first one's leftover state, so read from the top. A check that waits on a
timer (sections 3 and 7) allows 240 and 150 seconds; the timer's first run is
`OnBootSec=60s` and it then repeats every few minutes.

## Maintaining it

Checks match the product's words (`set up on a different machine`, `Not verified
stopped`), so a deliberate wording change fails here as well as in the Go tests;
update both. The fixture transcript is a repository fixture
(`internal/archive/testdata/claude-model-tokens.jsonl`): a change to its prompt text or
version needs the matching change in `guest.sh`.
