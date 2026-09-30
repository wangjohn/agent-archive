# Platform abstraction: one scheduler port and one OS value

> **Proposed.** Not implemented. It supersedes sections 3c to 3e and the "Linux PR list" of [portable-handoff-and-onboarding.md](portable-handoff-and-onboarding.md) for PRs 5 to 7. Linux PRs 1 to 4 (release artifacts, installer, Cursor paths, credential store) are in review or merged and are not re-planned here, except where this plan folds their OS switches into the new seam.

Status: revised 2026-09-30 after an independent design review (staff-engineer level) of the first draft. The review's blockers and majors are all addressed below; the first draft proposed a six-interface `Platform` bundle, which the review showed was over-built for everything except the scheduler.

## Why this document exists

About 95% of the code is platform-independent: the capture core, adapters, filtering, storage, retention, reader, stats and most of backfill. The remaining OS-specific code is small, but it is not in one place, and the Linux work so far has spread it further. If the systemd work simply adds "a Linux path" next to the launchd one, the repository ends up with two of everything and a growing set of `if goos == ...` branches. The goal is the opposite: Linux is the second implementation of one real port, and a third scheduler or operating system is a third implementation plus a conformance suite, not another round of edits through `cli`.

### What the code looks like today

Measured on `origin/main` at `61fb6ea`, with the open Linux PRs read from their branches:

1. **The scheduler is the only real coupling.** Twelve production files are scheduler-coupled: `env_defaults.go`, `cli.go`, `install_paths.go`, `setup_transaction.go`, `setup_preflight.go`, `status.go`, `uninstall.go`, `collector_env.go` (`launchdPath`), `hooks/install.go`, and `setupjournal`'s `journal.go`, `legacy.go` and `launchd.go`. The plist renderer and three parsers (about 185 lines, `hooks/install.go` 462 to 650) live in the package that edits app hook files.
2. **The job identity is a plist file path everywhere** (`JobState(plist)`, `Load(plist)`, `Journal.Plist`); a label is recovered by stripping a file name (`launchLabel`). Callers also act on jobs that are not this installation's current key: earlier labels and the prototype job (`com.agent-skills.skill-runs-upload`), used by setup, `status` (`installedCollectorPlist`) and `uninstall` alike.
3. **Job state is a public stringly-typed vocabulary.** `status --json` `background` has **six** values: `loaded`, `running`, `missing`, `unknown`, `another_installation`, and `broken` (added by `status` itself, `status.go:907`).
4. **OS switches are multiplying.** On `main` there is one runtime `GOOS` read (`cursorstore/snapshots.go:41`). With Linux PRs 3 and 4 there are eight or nine (`AppSupportDir`, `backfill.Environment.GOOS`, `Env.BackfillGOOS`, `discoverApplicationsFor`, `defaultTempDirs`, `credentialGOOS`, `OpenOptions.GOOS`, `StoreName`), all `goos string` parameters, plus compile-time pairs (birth time, Keychain, owner check, file store). An unknown `goos` silently means the Linux layout (`cursorstore.go:44`).
5. **macOS history is entangled with the shared flow.** Relabeling after pre-v0.1.0 builds and the prototype-job migration exist in the shared transaction and in `Journal` (`legacy`, `relabeled`, `more_relabeled`). A Linux backend never had earlier labels.
6. **The tests encode the shape.** 19 test files set `LoadLaunchAgent`, `UnloadLaunchAgent` or `JobState` directly; 61 of 129 `cli` test files inherit stand-ins through `testEnv`. Safety for the rest is a package variable (`runLaunchctl`) that `isolateProcessForTesting` replaces with a panic.

## Goals and non-goals

Goals:

- **One scheduler port** with a launchd and a systemd adapter and a shared conformance suite. Adding cron, a hook-triggered fallback or another OS is an adapter.
- **One typed OS value**, read in one place, replacing every `goos string` parameter, and one struct for OS paths built from it.
- macOS behavior stays byte-identical: existing plists, Keychain items, journals, `status --json` values, log files and every macOS string.
- No new interface unless it has a second real implementation or an external effect.

Non-goals: Windows (the code is Unix-only through `flock`, `syscall.Kill` and signals; this plan declares that with `//go:build unix`), local-only or folder backends, `wget` installs, libsecret, journald logging, building a cron or hook-triggered fallback (the port must allow one), and a terminals port (handoff's `termlaunch` already injects its OS and is unmerged; it stays handoff's business).

## Design

### Packages and import rules

```
internal/platform/             typed OS (the only reader of runtime.GOOS) and the Locations struct. Pure; no exec.
internal/scheduler/            the port: types, narrow interfaces, JobState, typed errors, Runner. Pure; no exec.
internal/scheduler/launchd/    adapter: plist codec, launchctl calls, all macOS scheduler history.
internal/scheduler/systemd/    adapter (PR 5b).
internal/scheduler/host/       chooses the adapter for an OS; owns the default Runner (process groups, timeouts).
internal/testutil/schedulertest/  fakes and the conformance suite (under testutil so deadcode and depguard already treat it as test code).
```

- Only `internal/scheduler/host` imports the adapters (an import-graph test, the pattern `capture` already uses). `cli` imports `scheduler` and `host`; `setupjournal` and `capture` import at most `scheduler` and `platform`, which are pure.
- Adapters are **not build-tagged**: `host` switches on the OS value at runtime, so every adapter compiles and is analyzed on every CI runner (deadcode runs only on macOS today).
- `hooks` returns to editing hook files only; `hooks.Change` stays where it is and `setupjournal` keeps importing it.

### The OS value and paths

```go
package platform

type OS string // Darwin, Linux; anything else is Unknown and fails closed

func Current() OS // the only place that reads runtime.GOOS

type Locations struct { /* values, not methods that shell out */ }
func NewLocations(os OS, home string, getenv func(string) string) Locations
```

`Locations` is a struct of computed values (Cursor state database, workspace storage, Claude desktop scratch and Codex documents as "absent on this OS" where they are, temp roots, protected folders, snapshot root). It is pure data over `(OS, home, getenv)`, so tests pass values, not mocks. `credentials.OpenOptions.GOOS`, `StoreName`, `AppSupportDir`, `backfill.Environment.GOOS`, `Env.BackfillGOOS`, `discoverApplicationsFor`, `defaultTempDirs` and `isMac()` take or derive from `platform.OS` and `Locations`; the tables Linux PRs 3 and 4 already wrote stay, keyed by the typed value. `Env.DiscoverApplications` and `termlaunch.Environment` stay the injected seams they are. File birth time stays a build-tagged pair (an interface would still need the tags). An unknown OS fails closed instead of defaulting to Linux.

### The scheduler port

Only the scheduler has external effects and two real implementations, so it is the one port. It is defined in domain terms and composed of narrow interfaces so each consumer depends on what it uses (`setupjournal` already defines its own small one).

```go
package scheduler

// Ref is opaque, adapter-minted and JSON-safe: a launchd label, a systemd unit stem.
type Ref string

// Installation is the identity input; the adapter derives the Ref from it
// (launchd: "com.agent-archive.collector", plus "." and the first 12 hex digits of
// sha256(Clean(DataHome)) for a non-default installation, exactly CollectorLabel today).
type Installation struct {
    DataHome string // canonical
    Default  bool   // the account's own installation; computed by cli (isDefault)
}

// Site is the per-call context: tests change the user home after the Env is built.
type Site struct{ UserHome string }

type JobSpec struct {
    Executable string
    Args       []string          // ["_collect"]
    DataHome   string
    Env        map[string]string // excludes AGENT_ARCHIVE_HOME; never carries credentials
    Interval   time.Duration     // 60s
    RunAtLoad  bool
    // Log paths are derived by the adapter from DataHome: collector.log, collector-error.log.
}

// Artifact is one thing a definition changes: a file for launchd and systemd.
// ID is "file:/abs/path" (a crontab backend would use "crontab:<user>").
type Artifact struct {
    ID    string
    After []byte
    Mode  os.FileMode
}

// Plan is pure: no disk reads. Shared code fills in Before and Existed by reading
// each Artifact and builds the hooks.Change the journal already stores.
type Plan struct {
    Ref       Ref
    Artifacts []Artifact
    Retire    []Retiree // earlier jobs (old labels, the prototype, another backend's job) to stop and remove
}

type Retiree struct {
    Backend   string
    Ref       Ref
    Artifacts []Artifact // as found; After empty means delete
    WasLoaded bool
}

type JobState string // loaded | running | missing | unknown | another_installation (status adds "broken")

type Problem struct{ Detail, Fix string }

type Status struct {
    State    JobState
    Program  string            // recovered from the definition on disk, for "is the binary still there"
    Env      map[string]string // recovered, without AGENT_ARCHIVE_HOME, so it round-trips into JobSpec.Env
    DataHome string
    Paths    []string          // what uninstall removes
    Problem  *Problem          // set for unknown and another_installation: what happened and what to do
    Degraded []string          // works, but not robustly (systemd: lingering is off)
}

type Definer interface {
    Name() string
    Ref(inst Installation) Ref
    Plan(site Site, inst Installation, spec JobSpec) (Plan, error)
    DefaultPATH() string // the PATH the job gets when its definition sets none
}
type Inspector interface {
    Inspect(ctx context.Context, site Site, ref Ref) Status // never errors for "cannot tell": that is State unknown plus Problem
    Installed(ctx context.Context, site Site, inst Installation) []Ref // this installation's own job and its aliases
}
type Controller interface { // change operations run on a bounded context that an interrupt never cancels
    Load(ctx context.Context, site Site, ref Ref) error   // loads what is on disk; systemd reloads its manager first
    Unload(ctx context.Context, site Site, ref Ref) error // returns *NotOwnedError or *IndeterminateError, which Commit rolls back on
}
type Scheduler interface{ Definer; Inspector; Controller }

type Runner func(ctx context.Context, name string, args ...string) (stdout []byte, err error)
```

What this fixes from the first draft:

- **Definitions are desired state, not files.** `Plan` is pure and never reads the disk; the shared transaction reads each artifact for `Before` and keeps producing the same `hooks.Change` the journal stores, so file journals do not change. Systemd's two files fit; a crontab or a registered task would be another artifact ID with an adapter-supplied applier.
- **Identity is an opaque `Ref` minted from an `Installation`.** `Load`, `Unload` and `Inspect` address anything setup, `status` and `uninstall` touch, including earlier labels and the prototype. `Installed` replaces the file scan (`previousCollectorPlists`) and the fallback in `installedCollectorPlist`; the macOS history lives only inside the launchd adapter.
- **`Available` is gone.** Cannot-tell is `State: unknown` with a `Problem` (launchd has no GUI domain; systemd has no user bus), one channel. `Degraded` carries "works while you are logged in": lingering off.
- **Ownership is the adapter's job.** `Inspect` returns `another_installation` when the registered definition is not ours (launchd: the loaded `path =` line; systemd: `FragmentPath`, both compared with `local.SameLocation`), and `Unload` refuses with `*NotOwnedError`. Shared code never compares labels or paths.
- **`Terms` shrinks to `Problem`.** The refusal states already carry the words: `Problem{Detail, Fix}` is produced by the adapter per site, pinned by goldens. Only a tiny noun set (manager, job noun) is shared, and never verbs or plurals.
- **Timeouts and interrupts are part of the contract.** `Inspect` is short and read-only (today's 2 s). Change operations get a bounded context (30 s) that a Ctrl-C or SIGTERM never cancels, because the journal handles half-applied work; the default runner in `host` owns process-group handling (`//go:build unix`) and passes the environment systemd needs (`XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`). Constructing a scheduler never executes anything, since the `_hook` runtime has a 2 s budget.
- **Concurrency.** The port is used under `setup.lock`; `Inspect` is read-only and safe next to a running collector.
- **Refresh.** `setup --refresh` (PR #172) re-renders a definition from the parsed program and environment of an existing one: `Status.Env` round-trips into `JobSpec.Env` by contract, and the `files_only` journal mode (no scheduler calls) stays in shared code.

**Which backend runs, and cross-backend moves.** `host` picks the platform default (launchd on darwin, systemd on Linux). Setup records the backend name in `config.json` (`background_backend`); **absent means launchd on darwin and systemd on Linux, frozen for all time**, so an old binary that rewrites `config.json` and drops the field cannot change meaning. `status`, `uninstall` and recovery use the recorded backend even when it is no longer usable. If setup ever picks a different backend than the recorded one, the transaction inspects and retires the recorded backend's job through **that** adapter (`Retiree.Backend`), so nothing is orphaned.

**When the scheduler is unreachable.** Today `uninstall` refuses on `unknown`. On Linux that will be common (SSH without a user bus, containers, a copied data directory) and must not make an install unremovable. Policy: uninstall refuses with the adapter's `Problem.Fix` and the exact manual steps, and proceeds to delete files and configuration only with an explicit flag (open decision O3).

### Persisted formats and compatibility

| Item | Rule |
| --- | --- |
| `setup-transaction.json` | Keep every existing field (`plist`, `legacy`, `relabeled`, `more_relabeled`, `changes`, `was_loaded`, and `files_only` from #172). Add optional `backend` (absent means launchd) and `job_ref` (absent means derived from the plist file name), and for other backends `definition_paths`. An old binary recovers a new launchd journal and a new binary reads old ones, with no converter and no dual-writing. Fixture journals written by the current code are replayed by a test. Config and journal loads use plain `json.Unmarshal`, so unknown fields are ignored. The window is tiny: a journal exists only during an interrupted setup. |
| `status --json` `background` | Values unchanged. `broken` stays a status-layer value on a wider type than `scheduler.JobState`. `docs/reference/json-output.md` and `docs/guides/troubleshooting.md` say "the launchd job": reworded, additive backend name optional. |
| Plist bytes, label, location, log paths | Byte-identical, pinned by goldens written **before** any code moves. |
| `_collect` | Unchanged; installed jobs depend on it. |
| Keychain service and JSON tags | Unchanged. |
| `config.json` | One optional field (`background_backend`); a Linux install writes it, existing macOS installs never do. |

### systemd, specified where it breaks

- **State map.** Timer `active (waiting)`: loaded. Service `activating` or `active` (a oneshot is `activating (start)`, not `active (running)`): running. Timer inactive with the unit files present: missing. `LoadState=not-found`: missing. `masked`, `bad-setting`, no user bus: unknown with a `Problem`. `FragmentPath` differing from ours: another_installation. Service `failed` while the timer is active: loaded. The map is written into the adapter and pinned by recorded output.
- **Load and Unload.** `Load` runs `daemon-reload`, then `enable --now` on the timer (the enable symlink is a third artifact outside the two unit files, so `Unload` is `disable --now` on the timer and a stop of the service, followed by `daemon-reload` after shared code deletes the files). A rollback to "did not exist" while unloaded leaves a stale unit in the manager unless the reload runs, so the adapter reloads whenever it changes artifacts.
- **Inspect reads what runs**, from `systemctl --user show`, not from the unit file, because drop-ins make them differ. `Environment=` in `show` output is space-separated and shell-quoted; the parser is fuzzed on a round trip, as is the unit renderer (`%` is a specifier, `$` expands in `ExecStart`, spaces in paths need quoting).
- **Timer.** `OnBootSec` plus `OnUnitActiveSec` (`OnUnitActiveSec` alone never fires until the service has run once), `AccuracySec=1s`, `Persistent`; `Type=oneshot`; overlap is prevented by the manager and by the collector's flock.
- **Logs.** `StandardOutput=append:` needs systemd 240 or newer; RHEL 8 ships 239. The adapter reports older systemd as a `Problem` on `Inspect` and setup refuses with a fix (open decision O4).
- **Recorded fixtures** of `systemctl show` output from systemd 239, 245 and 252 or newer back the conformance suite, and an opt-in real-manager job (see the controls below) runs it against a real user manager.

## Behavior-preservation controls

1. **Characterization first (5a-0).** A tests-only PR pins today's macOS behavior before any move. Existing tests already pin much (`setup_transaction_relabel_test`, `setup_transaction_recovery_test`, `status_binary_test`); inventory those first and add the gaps: the exact `launchctl` argument sequence and its ordering (`print gui/UID/label`, `bootstrap gui/UID plist`, `bootout gui/UID/label`; unload old, write files, retire legacy, load new), the refusal texts of `unknown` and `another_installation` at each of the four call sites, the `Restore` branches (`RecoveryBlockedError` wording, `--abandon-recovery`), `files_only` and refresh, the uninstall "kept" path, golden plists for the default and a non-default installation, fixture journals from the current code (including a relabel and a legacy case), and `parseJobState` over recorded `launchctl print` output. There are no setup or uninstall transcript goldens today, so this is new harness work.
2. **Architecture tests** (the repo pattern: a Go test plus a mirror depguard rule):
   - no `runtime.GOOS` outside `internal/platform` and the build-tagged files that already exist (an AST scan of non-test files);
   - only `internal/scheduler/host` imports the adapters;
   - `internal/scheduler` and `internal/platform` import no `os/exec`, `credentials` or `hooks`;
   - `os/exec` importers are an explicit, listed set (adapters, the default runner in `host`, `cursorstore` `getconf`, `pager`, `capabilities`, `handoff_launch`).
   A ban on the words `launchctl`, `plist` or `Keychain` is not used: they are legitimate user-facing wording and identifiers (`credentials.ErrKeychainLocked`).
3. **A conformance suite** in `internal/testutil/schedulertest`: the state matrix, ownership refusal with typed errors, idempotent unload, `Plan` purity and golden output, `Plan`/`Inspect` round trip, refresh round trip, no credential values in a definition, problem text for `unknown`. Every backend passes it over a fake `Runner` and recorded output; a third backend is done when it passes.
4. **A real-manager job.** An opt-in CI job (gated on an environment variable, on `ubuntu-latest`, which is a real VM with systemd) enables linger, sets `XDG_RUNTIME_DIR`, uses unique unit names, and runs the conformance suite against the real user manager. This replaces "one person's manual check" as the release gate for Linux.
5. **Fail-closed isolation is kept.** `cli` reads the scheduler through a package variable (`newScheduler`) that `TestMain` overwrites with a panicking one, exactly as `runLaunchctl` and `openCredentialStore` work today, so the 68 `cli` test files that build a bare `Env{}` cannot reach real `launchctl` or `systemctl`. A nil `Env` field means the real host; `cli.Run` resolves it, and `cmd` stays a wrapper. `_hook` and cloud mode never construct a scheduler.
6. **Fuzz.** The plist parsers have no fuzz target today; moving them (5a-3) adds a round-trip target. The fuzz job requires 13 or more targets and has 14, so the count cannot drop.

## Work plan

Every PR follows the standing process: a separate implementer (worktree, fresh context), a separate reviewer who mutation-tests, a second review after any fix round that touches correctness, `main` merged in before review, and CI green on the final head. Implementers run the pinned Levenshtein lint and both deadcode passes locally before pushing. Nothing merges without the owner's say-so, except docs-only plan PRs.

**In-flight work.** About a dozen open PRs add fields to `Env` or edit `setup_transaction.go`, `status.go`, `uninstall.go`, `setup_preflight.go`, `env_defaults.go`, `journal.go` and `config.go` (#172 setup `--refresh`, #152, #156, #163, #162, #151, #165, #169, #148). The seam PRs rebase on whatever has merged, and #172 in particular lands or is rebased first, because it adds a journal mode, launchctl timeouts and `refreshPlist` that the seam must carry.

| PR | Scope | Depends on | Owns |
| --- | --- | --- | --- |
| **P0** | This document | none | `dev/proposals/platform-abstraction.md`, a pointer in the older plan |
| **5a-0** | Characterization (tests only) | #172 merged or rebased | new tests and testdata |
| **5a-1** | Journal: additive optional `backend` and `job_ref`, fixture replay, no interface change | 5a-0 | `internal/setupjournal/journal.go` and tests |
| **5a-2** | The seam in place: an unexported `scheduler` interface in `cli` with a `launchdScheduler` wrapper over the current functions; replaces `Env.JobState`, `LoadLaunchAgent`, `UnloadLaunchAgent` and the `Env.jobState` hidden default; migrates the 19 test files onto one fake; keeps the fail-closed package variable | 5a-1 | `internal/cli/{cli,env_defaults,setup_preflight,setup_transaction,status,uninstall}.go`, the tests, `isolation_test.go` |
| **5a-3** | Move: the plist codec (about 185 lines) and the wrapper into `internal/scheduler/launchd` and the port types into `internal/scheduler`, pure code motion plus a fuzz target; `host` | 5a-2 | `internal/hooks/install.go` (the moved lines only), `internal/scheduler/**`, `internal/testutil/schedulertest` |
| **5a-4** | Identity and history: `Ref`, `Installation`, `Site`, `Artifact`, `Plan`, `Installed`, `Retiree`, typed errors and `Problem`; macOS history (`previousCollectorPlists`, `PlanRelabel`, `PlanLegacyMigration`, `isCollectorLabel`) moves behind the launchd adapter; conformance suite | 5a-3 | the launchd adapter, `internal/setupjournal/{legacy,launchd}.go`, `install_paths.go` |
| **5a-5** | One OS value: `platform.OS`, `Locations`; fold every `goos string` from Linux PRs 3 and 4; unknown OS fails closed; `//go:build unix` on `flock` users | 5a-2 merged (both edit `Env` and `testEnv`); otherwise disjoint | `internal/platform`, `credentials` options and wording, `internal/cli/{credential_words,capabilities,backfill}.go`, `internal/backfill`, `internal/cursorstore` |
| **5b** | systemd adapter and its conformance run, recorded fixtures, `host` selection, `background_backend` in config, uninstall policy, real-manager CI job | 5a-4, 5a-5 | `internal/scheduler/systemd`, `host`, config field, `.github/workflows/test.yml` job |
| **5c** | Linux job environment and end-to-end: `DefaultPATH`, forward `XDG_CONFIG_HOME` to the job, Cursor snapshot root under a per-user cache directory, machine-id clone warning, the live acceptance run | 5b, 5a-5 | `collector_env.go`, `Locations` snapshot root, docs |
| **7** | Terminology and "Linux supported" docs, `multiple-macs.md` to `multiple-machines.md` with every inbound link, FAQ, README, `CONTRIBUTING.md`, `dev/specs/archive.md`, `json-output.md` and `troubleshooting.md` wording | 5b, 5c live-verified | docs, help strings, goldens |

Fixed names: `platform.OS`, `platform.Current`, `platform.Locations`, `platform.NewLocations`, `scheduler.Scheduler` (composed of `Definer`, `Inspector`, `Controller`), `scheduler.Ref`, `scheduler.Installation`, `scheduler.Site`, `scheduler.JobSpec`, `scheduler.Artifact`, `scheduler.Plan`, `scheduler.Retiree`, `scheduler.JobState` (five constants), `scheduler.Status`, `scheduler.Problem`, `scheduler.NotOwnedError`, `scheduler.IndeterminateError`, `scheduler.Runner`, `host.Default`, `schedulertest.RunConformance`, `schedulertest.Model`.

Dropped from the first draft: a `Platform` bundle, `Apps`, `Files` and `Terminals` interfaces, and PR "T". A `BirthSource: modified` metadata change (so archive consumers can tell a Linux mtime from a creation time) is a separate, later metadata PR under `dev/maintainers/versions.md`, not part of a no-behavior-change refactor.

## Open decisions for the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| O1 | Persist the chosen scheduler backend in `config.json`? | Yes; absent means launchd on darwin and systemd on Linux, frozen. |
| O2 | Where do Cursor database snapshots live on Linux? `/tmp` is shared, the root name is predictable so another local user can block it, and a scheduled job does not inherit the shell's `TMPDIR`. | A per-user, disk-backed cache directory (`$XDG_CACHE_HOME`, default `~/.cache`, `agent-archive/cursor-snapshots`, 0700). macOS unchanged. |
| O3 | What does `uninstall` do when the scheduler cannot answer? | Refuse with the adapter's fix and the manual steps; proceed with an explicit flag that deletes files and configuration without asking the scheduler. |
| O4 | systemd older than 240 (no `append:` logging)? | Refuse with a fix message in the first release; revisit if a target distro needs it. |
| O5 | Forward `XDG_CONFIG_HOME` to the job, or record the resolved Cursor database path at setup? | Forward it (the AWS variables' pattern), so backfill and the collector cannot disagree. |
| O6 | Land or rebase around #172 first? | Land #172 first; the seam is smaller with its journal mode, timeouts and refresh already in place. |

## Risks

- **The port is still too launchd-shaped.** The systemd rows above are the design check; the reviewer of each PR must try to break the port with a crontab and a hook-triggered fallback (no definition file, no readable `Program`).
- **5a-2 is large.** It is the seam swap under 12 production files and 19 test files. It is deliberately code-shape only, with the current functions behind the wrapper; the code motion is 5a-3.
- **Recovery across an upgrade.** Fixture journals from shipped versions and a replay test are release-blocking, though the journal exists only during an interrupted setup.
- **Real systemd is unseen from a Mac.** Lingering, the user bus, `append:` logging, the Cursor Linux path and `cursor-agent` hooks (reported failing silently on Linux) stay unverified until 5b's real-manager job and 5c's live run; docs do not advertise Linux before those pass.
- **Rebase cost.** The number of open PRs touching `Env` is the main schedule risk, which is why 5a-2 rebases on whatever has merged and why O6 recommends landing #172 first.

## Definition of done for "platform-agnostic"

Everything except `internal/scheduler/host` and the adapter directories compiles and passes its tests against `internal/scheduler` and `internal/platform` alone, using the schedulertest model. Adding an operating system or a second scheduler touches only its own adapter directory, `host`, the OS value, and the docs.
