# Platform abstraction: one scheduler port and one OS value

> **Proposed.** Not implemented. It supersedes sections 3c to 3e and the "Linux PR list" of [portable-handoff-and-onboarding.md](portable-handoff-and-onboarding.md) for PRs 5 to 7. Linux PRs 1 to 4 (release artifacts, installer, Cursor paths, credential store) are merged or in review and are not re-planned here, except where this plan folds their OS switches into the new seam.

Status: revised twice on 2026-09-30 after two rounds of independent design review (staff-engineer level). The first draft proposed a six-interface `Platform` bundle, which the review showed was over-built for everything except the scheduler; the second round fixed the port signatures (retirement, identity, wording, the runner). All blockers and majors are addressed below, and the six decisions the first draft left open are resolved and folded into the design (each recommendation was re-checked against the code; three changed, see "Decisions taken" at the end).

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
func NewLocations(os OS, home string, getenv func(string) string, deps LocationDeps) Locations
```

`Locations` is a struct of computed values (Cursor state database, workspace storage, Claude desktop scratch and Codex documents as "absent on this OS" where they are, temp roots, protected folders, snapshot root). It is data over `(OS, home, getenv)`, so tests pass values, not mocks. The two values that need the system today stay injected functions in `LocationDeps` (the macOS per-user temp directory from `getconf DARWIN_USER_TEMP_DIR`, and symlink resolution for the protected-folder list), with their real implementations supplied by `cursorstore` and `host`, so `internal/platform` keeps its no-exec rule. `credentials.OpenOptions.GOOS`, `StoreName`, `AppSupportDir`, `backfill.Environment.GOOS`, `Env.BackfillGOOS`, `discoverApplicationsFor`, `defaultTempDirs` and `isMac()` take or derive from `platform.OS` and `Locations`; the tables Linux PRs 3 and 4 already wrote stay, keyed by the typed value. `Env.DiscoverApplications` and `termlaunch.Environment` stay the injected seams they are. File birth time stays a build-tagged pair (an interface would still need the tags). An unknown OS fails closed instead of defaulting to Linux.

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

// Artifact is one thing a definition changes. Version 1 of the port is file-only:
// ID is "file:/abs/path". A backend whose definition is not a file (crontab, a
// registered task) needs an Applier on Controller to read Before and apply After;
// that is deliberately not built until such a backend exists.
type Artifact struct {
    ID    string
    After []byte
    Mode  os.FileMode
}

// Plan is pure: no disk reads and no scheduler calls. Shared code fills in Before
// and Existed by reading each Artifact and builds the hooks.Change the journal
// already stores.
type Plan struct {
    Ref       Ref
    Artifacts []Artifact
}

// Retiree is built by SHARED code, not by an adapter, so it can name another
// backend's job: for each relevant adapter it takes the refs from Installed, calls
// Inspect for State (WasLoaded) and Paths, and reads the Before bytes. It lives in
// the journal, and setupjournal resolves Backend to that backend's Controller.
type Retiree struct {
    Backend   string
    Ref       Ref
    Artifacts []Artifact // as found; After empty means delete
    WasLoaded bool
}

type JobState string // loaded | running | missing | unknown | another_installation (status adds "broken")

// Problem is structured facts, not a sentence. Today the another_installation text
// has four different wordings (setup, preflight, uninstall, unload); the templates
// stay in cli, pinned by the 5a-0 goldens, and are filled from these facts and Words.
type Problem struct {
    Kind       string // "not_owned" | "cannot_tell" | ...
    Ref        Ref
    LoadedFrom string // the definition the manager loaded, for not_owned
    Expected   string // the definition this installation expects
    Fix        string // adapter-supplied next step
}

// Words is the small shared noun set. Nouns only: no verbs, no plurals.
type Words struct{ Manager, Job, Definition, Tool string } // "launchd", "LaunchAgent", "plist", "launchctl"

type Status struct {
    State    JobState
    Defined  bool              // a definition for this Ref exists on disk
    Program  string            // read from the definition on disk, whatever State is
    Env      map[string]string // same source; without AGENT_ARCHIVE_HOME, so it round-trips into JobSpec.Env
    DataHome string
    Paths    []string          // what uninstall removes
    Problem  *Problem          // set for unknown and another_installation
    Degraded []string          // works, but not robustly (systemd: lingering is off; a drop-in overrides the unit)
    // DefinitionErr is set when the definition exists but cannot be read, which refresh
    // (setup --refresh) distinguishes from absent.
}

type Definer interface {
    Name() string
    Words() Words
    Ref(inst Installation) Ref
    Plan(site Site, inst Installation, spec JobSpec) (Plan, error)
    DefaultPATH() string // the PATH the job gets when its definition sets none
}
type Inspector interface {
    // Inspect never errors for "cannot tell": that is State unknown plus a Problem.
    // Program, Env and Defined come from the definition on disk regardless of State.
    Inspect(ctx context.Context, site Site, ref Ref) Status
    // Installed returns this installation's own job first, then its aliases (earlier labels,
    // a prototype job). The error carries what blocks setup today (an unrecognized prototype
    // definition, "preserve it and resolve it before setup").
    Installed(ctx context.Context, site Site, inst Installation) ([]Ref, error)
}
type Controller interface { // change operations run on a bounded context that an interrupt never cancels
    // Load loads what is on disk and must not be called when the definition was rolled
    // back to "did not exist"; systemd reloads its manager first.
    Load(ctx context.Context, site Site, ref Ref) error
    // Unload returns *NotOwnedError or *IndeterminateError, which Commit and Restore roll back on;
    // nil when the job is not loaded.
    Unload(ctx context.Context, site Site, ref Ref) error
}
type Scheduler interface{ Definer; Inspector; Controller }

// Runner returns the combined stdout and stderr, as CombinedOutput does today: parseJobState
// finds "Could not find service" in the combined output of a failed launchctl print, and
// bootstrap and bootout error messages quote it.
type Runner func(ctx context.Context, name string, args ...string) (output []byte, err error)
```

What this fixes from the first draft:

- **Definitions are desired state, not files.** `Plan` is pure and never reads the disk; the shared transaction reads each artifact for `Before` and keeps producing the same `hooks.Change` the journal stores, so file journals do not change. Systemd's two files fit; a crontab or a registered task would be another artifact ID with an adapter-supplied applier.
- **Identity is an opaque `Ref` minted from an `Installation`.** `Load`, `Unload` and `Inspect` address anything setup, `status` and `uninstall` touch, including earlier labels and the prototype. `Installed` replaces the file scan (`previousCollectorPlists`) and the fallback in `installedCollectorPlist` (own first, then aliases; `Status.Defined` carries the "does the definition exist" question that fallback keys on); the macOS history lives only inside the launchd adapter. Retirement is planned by shared code from `Installed` and `Inspect`, because it needs the disk and the manager and can span backends.
- **`Available` is gone.** Cannot-tell is `State: unknown` with a `Problem` (launchd has no GUI domain; systemd has no user bus), one channel. `Degraded` carries "works while you are logged in": lingering off.
- **Ownership is the adapter's job.** `Inspect` returns `another_installation` when the registered definition is not ours (launchd: the loaded `path =` line; systemd: `FragmentPath`, both compared with `local.SameLocation`), and `Unload` refuses with `*NotOwnedError`. Shared code never compares labels or paths.
- **Wording stays byte-identical through structured facts.** `Problem` carries facts, `Words` a four-noun set, and the four existing `another_installation` sentences (setup, preflight, uninstall, unload) stay as templates in `cli`, pinned by the 5a-0 goldens. If a single sentence per state is ever accepted instead, the goldens change deliberately.
- **Timeouts and interrupts are part of the contract.** `Inspect` is short and read-only (today's 2 s). Change operations get a bounded context (30 s) that a Ctrl-C or SIGTERM never cancels, because the journal handles half-applied work; the default runner in `host` owns process-group handling (`//go:build unix`) and passes the environment systemd needs (`XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`). Constructing a scheduler never executes anything, since the `_hook` runtime has a 2 s budget.
- **Concurrency.** The port is used under `setup.lock`; `Inspect` is read-only and safe next to a running collector.
- **Refresh.** `setup --refresh` (PR #172) re-renders a definition from the parsed program and environment of an existing one: `Status.Env` round-trips into `JobSpec.Env` by contract (`AGENT_ARCHIVE_HOME` stripped, as #172's `refreshPlist` already does), and the `files_only` journal mode (no scheduler calls) stays in shared code. Interval and run-at-load are not recovered, so a hand-edited definition is normalized on refresh, as today.

**Which backend runs, and cross-backend moves.** The chosen backend is recorded, so an installed job is always addressed through the adapter that created it. `host` picks the platform default (launchd on darwin, systemd on Linux). Setup records the backend name in `config.json` (`background_backend`); **absent means launchd on darwin and systemd on Linux, frozen for all time**, so an old binary that rewrites `config.json` and drops the field cannot change meaning. `status`, `uninstall` and recovery use the recorded backend even when it is no longer usable. If setup ever picks a different backend than the recorded one, the transaction inspects and retires the recorded backend's job through **that** adapter (`Retiree.Backend`), so nothing is orphaned. (As built in 5b-2: setup leaves launchd out of `config.json`, which the absent rule already means, so a macOS file never changes; and while each system has one backend, a recorded one that differs is another system's, with no job here to retire, so that retirement waits for a system with two.)

**When the scheduler is unreachable.** Today `uninstall` refuses on `unknown`. On Linux that will be common (SSH without a user bus, containers, a copied data directory) and must not make an install unremovable. Policy: `uninstall` refuses with the adapter's `Problem.Fix` and the exact manual steps (for systemd, the `systemctl --user disable --now` command to run from a session that has the user bus). A new flag (working name `--skip-scheduler`, final spelling settled in the 5b PR, with help text and a CLI reference entry) proceeds anyway: it attempts the stop best-effort, deletes the definition files and configuration, prints the same manual command, and says in its summary that the job was not verified stopped. It is a flag and not automatic because deleting a definition under a loaded timer leaves the job running, and archiving, after the user was told it was uninstalled.

### Persisted formats and compatibility

| Item | Rule |
| --- | --- |
| `setup-transaction.json` | Keep every existing field (`plist`, `legacy`, `relabeled`, `more_relabeled`, `changes`, `was_loaded`, and `files_only` from #172). Add optional `backend` (absent means launchd) and `job_ref` (absent means derived from the plist file name), and for other backends `definition_paths`; they arrive with the identity change (5a-3), not before, so no field is written that nothing reads and `plist` and `job_ref` cannot disagree. An old binary recovers a new launchd journal and a new binary reads old ones, with no converter and no dual-writing. Fixture journals written by the current code are replayed by a test. Config and journal loads use plain `json.Unmarshal`, so unknown fields are ignored. The window is tiny: a journal exists only during an interrupted setup. |
| `status --json` `background` | Values unchanged. `broken` stays a status-layer value on a wider type than `scheduler.JobState`. `docs/reference/json-output.md` and `docs/guides/troubleshooting.md` say "the launchd job": reworded, additive backend name optional. |
| Plist bytes, label, location, log paths | Byte-identical, pinned by goldens written **before** any code moves. |
| `_collect` | Unchanged; installed jobs depend on it. |
| Keychain service and JSON tags | Unchanged. |
| `config.json` | One optional field (`background_backend`); a Linux install writes it, existing macOS installs never do. |

### systemd, specified where it breaks

- **State map.** Timer `active (waiting)`: loaded. Service `activating` or `active` (a oneshot is `activating (start)`, not `active (running)`): running. Timer inactive with the unit files present: missing. `LoadState=not-found`: missing. `masked`, `bad-setting`, no user bus: unknown with a `Problem`. `FragmentPath` differing from ours: another_installation. Service `failed` while the timer is active: loaded. The map is written into the adapter and pinned by recorded output.
- **Load and Unload.** `Load` runs `daemon-reload`, then `enable --now` on the timer (the enable symlink is a third artifact outside the two unit files, so `Unload` is `disable --now` on the timer and a stop of the service, followed by `daemon-reload` after shared code deletes the files). A rollback to "did not exist" while unloaded leaves a stale unit in the manager unless the reload runs, so the adapter reloads whenever it changes artifacts.
- **`State` comes from `systemctl --user show`; `Program` and `Env` come from the unit file we wrote**, the same source as launchd, so refresh never bakes a drop-in's values into a new unit. A drop-in that overrides the unit is reported in `Degraded`. `Environment=` in the unit file and in `show` output is space-separated and shell-quoted; the parser is fuzzed on a round trip, as is the unit renderer (`%` is a specifier, `$` expands in `ExecStart`, spaces in paths need quoting).
- **Timer.** `OnBootSec` plus `OnUnitActiveSec` (`OnUnitActiveSec` alone never fires until the service has run once), `AccuracySec=1s`; `Type=oneshot`; overlap is prevented by the manager and by the collector's flock.
- **Logs.** `StandardOutput=append:` needs systemd 240 or newer; RHEL 8 ships 239. The adapter reports older systemd as a `Problem` on `Inspect` and setup refuses with a fix. Decided: the first release does not support systemd older than 240 (the RHEL 8 family at 239, Ubuntu 18.04 at 237, CentOS 7 at 219); Debian 10 (241) and Ubuntu 20.04 (245) and later are fine. A shell-wrapper redirect that avoids `append:` was rejected for now because it adds an `sh` dependency and a second quoting surface; revisit only if a target distro needs it.
- **Recorded fixtures** of `systemctl show` output from systemd 239, 245 and 252 or newer back the conformance suite, and an opt-in real-manager job (see the controls below) runs it against a real user manager.

### Linux job environment and the Cursor snapshot location

A scheduled job does not inherit the interactive shell's environment, and two Cursor-related paths depend on it. Both are decided:

- **Forward `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` to the job, on Linux only, when set and absolute.** Backfill run from a shell and the scheduled collector must resolve the same Cursor database, and a recorded path would go stale silently, whereas a recorded variable is covered by the drift warning that already compares the job's recorded AWS files with the shell's (`awsFilesDrift`), extended to these two. Two consequences: this is provider-independent (today `buildCollectorEnvironment` returns nothing for R2, so the builder changes on Linux), and it never applies on macOS, where Cursor's path ignores XDG and forwarding would change existing plists, which must stay byte-identical.
- **On Linux, Cursor database copies live under `<XDG_CACHE_HOME or ~/.cache>/agent-archive/cursor-snapshots`** (mode 0700, owned by the user; the existing ownership and mode checks apply unchanged; `~` is the account home from the user database, not `$HOME`, as for `installation.accountHome`). The root also gets a `CACHEDIR.TAG` so backup tools that honor the convention skip it. It is not `/tmp` (shared, so a predictable name can be pre-created by another local user to block capture permanently; often tmpfs, which would put a multi-gigabyte copy in RAM; and not what a scheduled job sees) and not the data directory (`cursorstore/snapshots.go` deliberately keeps a copy of every Cursor chat, including projects that are not archived, out of the archive home, which may be backed up or synced). Because `XDG_CACHE_HOME` is forwarded, the shell and the job agree on the root, so the stale-copy sweep sees one directory. macOS is unchanged (the per-user temporary directory from `getconf DARWIN_USER_TEMP_DIR`). `Locations` exposes the root; work happens in 5c.

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
5. **`status` surfaces `Degraded`.** A row note in the human output and an additive `background_warnings` array in `status --json` (documented in `json-output.md`), so "lingering is off" is not invisible.
6. **Fail-closed isolation is kept.** `cli` reads the scheduler through a package variable (`newScheduler`) that `TestMain` overwrites with a panicking one, exactly as `runLaunchctl` and `openCredentialStore` work today, so the 68 `cli` test files that build a bare `Env{}` cannot reach real `launchctl` or `systemctl`. A nil `Env` field means the real host; `cli.Run` resolves it, and `cmd` stays a wrapper. `_hook` and cloud mode never construct a scheduler.
7. **Fuzz.** The plist parsers have no fuzz target today; moving them (5a-2) adds a round-trip target. The fuzz job requires 13 or more targets and has 14, so the count cannot drop.

## Work plan

Every PR follows the standing process: a separate implementer (worktree, fresh context), a separate reviewer who mutation-tests, a second review after any fix round that touches correctness, `main` merged in before review, and CI green on the final head. Implementers run the pinned Levenshtein lint and both deadcode passes locally before pushing. Nothing merges without the owner's say-so, except docs-only plan PRs.

**In-flight work.** About a dozen open PRs add fields to `Env` or edit `setup_transaction.go`, `status.go`, `uninstall.go`, `setup_preflight.go`, `env_defaults.go`, `journal.go` and `config.go` (#172 setup `--refresh`, #152, #156, #163, #162, #151, #165, #169, #148). #172 in particular adds a journal mode (`files_only`), launchctl timeouts and `refreshPlist` that the seam must carry, but it is stacked four deep (#172 on #165 on #156 on #152) and waits on owner merge decisions, so the plan does not gate on it. **5a-0 starts on `main` now** and characterizes what exists there; the behavior #172 adds gets a characterization addendum when its stack lands. **5a-1 starts after that stack merges**; if it has not merged when 5a-1 is ready, 5a-1 is stacked on #172's branch and retargeted on merge, the repository's existing practice for PRs that edit the same files.

| PR | Scope | Depends on | Owns |
| --- | --- | --- | --- |
| **P0** | This document | none | `dev/proposals/platform-abstraction.md`, a pointer in the older plan |
| **5a-0** | Characterization (tests only) of the behavior on `main`; an addendum for `files_only`, timeouts and refresh once #172 lands | none | new tests and testdata |
| **5a-1** | The seam in place, `Ref`- and `Site`-keyed from the start (launchd's `Ref` is the label `launchLabel(plist)` yields today): an unexported `scheduler` interface in `cli` with a `launchdScheduler` wrapper over the current functions; replaces `Env.JobState`, `LoadLaunchAgent`, `UnloadLaunchAgent` and the `Env.jobState` hidden default (a test-only behavior change, called out); migrates the 19 test files onto one fake that hides identity (state, load and unload closures); keeps the fail-closed package variable | 5a-0; #172's stack merged, or stacked on #172's branch | `internal/cli/{cli,env_defaults,setup_preflight,setup_transaction,status,uninstall}.go`, the tests, `isolation_test.go` |
| **5a-2** | Move: the plist codec (about 185 lines) and the wrapper into `internal/scheduler/launchd` and the port types into `internal/scheduler`, pure code motion plus a fuzz target; `host` and the default `Runner` (combined output, process groups, timeouts) | 5a-1 | `internal/hooks/install.go` (the moved lines only), `internal/scheduler/**`, `internal/testutil/schedulertest` |
| **5a-3** | Desired state and history: `Plan`, `Artifact`, `Installed` (with errors), `Status.Defined`, `Retiree` built by shared code, typed errors, structured `Problem` and `Words`, the journal's additive `backend` and `job_ref`, and `setupjournal` resolving a backend name to its `Controller`; macOS history (`previousCollectorPlists`, `PlanRelabel`, `PlanLegacyMigration`, `isCollectorLabel`) moves behind the launchd adapter; the conformance suite | 5a-2 | the launchd adapter, `internal/setupjournal/**`, `install_paths.go`, fixture replay tests |
| **5a-4** | One OS value: `platform.OS`, `Locations` with injected `LocationDeps`; fold every `goos string` from Linux PRs 3 and 4; unknown OS fails closed; `//go:build unix` on `flock` users | 5a-1 merged (both edit `Env` and `testEnv`); otherwise disjoint | `internal/platform`, `credentials` options and wording, `internal/cli/{credential_words,capabilities,backfill}.go`, `internal/backfill`, `internal/cursorstore` |
| **5b** | systemd adapter and its conformance run, recorded fixtures, `host` selection, `background_backend` in config, the `uninstall` policy and flag, `Degraded` in `status`, real-manager CI job | 5a-3, 5a-4 | `internal/scheduler/systemd`, `host`, config field, `.github/workflows/test.yml` job |
| **5c** | Linux job environment and end-to-end: `DefaultPATH`, forward `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` to the job (Linux only), Cursor snapshot root under the per-user cache directory, machine-id clone warning, the live acceptance run | 5b, 5a-4 | `collector_env.go`, `Locations` snapshot root, docs |
| **7** | Terminology and "Linux supported" docs, `multiple-macs.md` to `multiple-machines.md` with every inbound link, FAQ, README, `CONTRIBUTING.md`, `dev/specs/archive.md`, `json-output.md` and `troubleshooting.md` wording | 5b, 5c live-verified | docs, help strings, goldens |

Fixed names: `platform.OS`, `platform.Current`, `platform.Locations`, `platform.NewLocations`, `scheduler.Scheduler` (composed of `Definer`, `Inspector`, `Controller`), `scheduler.Ref`, `scheduler.Installation`, `scheduler.Site`, `scheduler.JobSpec`, `scheduler.Artifact`, `scheduler.Plan`, `scheduler.Retiree`, `scheduler.JobState` (five constants), `scheduler.Status` (with `Defined`), `scheduler.Problem`, `scheduler.Words`, `scheduler.NotOwnedError`, `scheduler.IndeterminateError`, `scheduler.Runner`, `host.Default`, `schedulertest.RunConformance`, `schedulertest.Model`.

Dropped from the first draft: a `Platform` bundle, `Apps`, `Files` and `Terminals` interfaces, and PR "T". A `BirthSource: modified` metadata change (so archive consumers can tell a Linux mtime from a creation time) is a separate, later metadata PR under `dev/maintainers/versions.md`, not part of a no-behavior-change refactor.

## Decisions taken

Each recommendation from the first draft was checked against the code before being adopted. Where it changed, the reason is here.

| Question | Decision | Where |
| --- | --- | --- |
| Persist the chosen scheduler backend in `config.json`? | Yes; absent means launchd on darwin and systemd on Linux, frozen for all time. Unchanged. | "Which backend runs" |
| `uninstall` when the scheduler cannot answer? | Refuse with the fix and manual steps; a new explicit flag proceeds. **Changed:** the draft said the flag "stops the unit if it can", which is contradictory (the flag exists for when it cannot); it now attempts the stop best-effort, then warns that the job may keep running. | "When the scheduler is unreachable" |
| systemd older than 240? | Refuse in the first release. Unchanged; the affected distros are now named. | "systemd, specified where it breaks" |
| Where do Cursor snapshots live on Linux? | A per-user cache directory. **Refined:** the draft's `$XDG_CACHE_HOME` default has the same shell-versus-job divergence as `TMPDIR`, so `XDG_CACHE_HOME` is forwarded to the job, and the doc records why the data directory is not used (the snapshots code keeps chat copies out of a home that may be backed up or synced). | "Linux job environment and the Cursor snapshot location" |
| Forward `XDG_CONFIG_HOME` or record the Cursor database path? | Forward it. **Refined:** Linux only (macOS plists must stay byte-identical and its Cursor path ignores XDG), provider-independent, covered by the existing drift warning. | same |
| Land #172 first? | **Changed:** no. #172 is the top of a four-deep stack waiting on owner merge decisions, so gating on it would block the whole series. 5a-0 starts on `main` now; 5a-1 starts after the stack merges or stacks on #172's branch. | "In-flight work" |

## Risks

- **The port is still too launchd-shaped.** The systemd rows above are the design check; the reviewer of each PR must try to break the port with a crontab and a hook-triggered fallback (no definition file, no readable `Program`).
- **5a-1 is large.** It is the seam swap under 12 production files and 19 test files. It is deliberately code-shape only, keyed by `Ref` and `Site` from the start so the tests are migrated once, with the current functions behind the wrapper; the code motion is 5a-2 and the desired-state model is 5a-3.
- **Recovery across an upgrade.** Fixture journals from shipped versions and a replay test are release-blocking, though the journal exists only during an interrupted setup.
- **Real systemd is unseen from a Mac.** Lingering, the user bus, `append:` logging, the Cursor Linux path and `cursor-agent` hooks (reported failing silently on Linux) stay unverified until 5b's real-manager job and 5c's live run; docs do not advertise Linux before those pass.
- **Rebase cost.** The number of open PRs touching `Env` is the main schedule risk, which is why 5a-0 starts on `main` immediately and 5a-1 rebases on whatever has merged, or stacks on #172.

## 5a-3 as built

5a-3 landed in four stacked pull requests (desired state and typed errors, the conformance suite and `schedulertest.Model`, `Installed` and the macOS history, the journal's `backend` and `job_ref`). Where the code proved the signatures above wrong, it differs in these ways:

- **`Inspector.Definition(site, ref) Status`** is added. `setup --refresh` reads a definition without asking the manager (a refresh whose definition it leaves alone runs no `launchctl` at all, pinned by the 5a-0 characterization), and `Inspect` always asks. `Definition` is `Inspect` without the state.
- **`Status.DefinitionErr`** is the error of the read, or of the first part of the definition that could not be parsed (the program before the environment), with what could be read still set: refresh reports an unreadable program, leaves a definition that already runs the new executable alone, and only then reports an unreadable environment, as before.
- **`Installed` returns `[]Job`** (`Ref` and an `Alias`: `EarlierLabel` or `Prototype`, empty for the installation's own job), not `[]Ref`. Which alias a job is decides the wording of setup's refusals, whether a job another installation runs blocks setup (the prototype's) or is skipped (an earlier label's), and which journal field records it; `status` and `uninstall` must skip the prototype. It returns the jobs it recognized together with the error that blocks setup, so `status` and `uninstall`, which ignore that error, keep working.
- **`Retiree` has an `Alias`**, for the same reason, and its artifacts are the definition as found (`After` is what recovery puts back), which retiring deletes.
- **`Definer.Locate(definition) (Site, Ref, error)`** is added. The journal names a job by the path of its definition and a journal is recovered under whichever `$HOME` the next setup has, so recovery needs the site and the job a recorded path names; that is adapter knowledge (`<home>/Library/LaunchAgents/<label>.plist`). `job_ref`, when recorded, must be the ref the path names: a release without the field acts on the path alone, so a journal where they disagree (only a hand edit writes one) is refused, its jobs untouched, rather than driven one way by one release and another by the next. Every job a journal drives (the collector's, and each retired job that was loaded; one that was not is never asked about, as before) is resolved before anything is changed, so `Commit` refuses such a journal before recording it and recovery before putting any file back. A files-only journal drives no job and resolves none.
- **`NotOwnedError` and `IndeterminateError`** carry `Words` and `Problem`, so their text is the words the untyped errors always said. `Problem.Kind` is a defined string type.
- **The launchd `Plan` refuses a spec that is not the collector** (`_collect` every minute, at load) rather than render it as something else; nothing else runs under launchd yet.
- **`setupjournal` resolves a backend by name** through `Backends`, a `func(name string) (scheduler.Scheduler, error)`, so it uses the whole port (`Inspect`, `Load`, `Unload`, `Locate`, `Words`) and not `Controller` alone. A journal that names a backend the system does not have is refused, its jobs untouched.
- **`cli` no longer imports the launchd adapter**; only `host` does (`TestOnlyHostImportsAdapters`, and its depguard mirror, lost their `cli` entry).
- `DefaultPATH` is wired through the collector environment now (`collectorPath`, `collectorEnvironmentProblems`); 5c read the Linux value from a live manager and kept it (see "5c as built").

## 5b-1 as built

5b-1 adds `internal/scheduler/systemd` alone: a pure adapter over a `Runner`, not wired into `host`, `setup`, `status`, `uninstall` or `config` (5b-2). Where it settled what the sections above left open:

- **Refs and files.** `agent-archive-collector` for the default installation and `agent-archive-collector-<12 hex>` (the digest `CollectorLabel` takes) otherwise; a job is `<ref>.service` and `<ref>.timer` in the user unit directory. `Plan` returns the service, then the timer, both mode 0600: the unit convention is 0644 and neither holds a credential, but the environment may hold a proxy address with a user name and password, launchd's plists are private too, and the user manager runs as the user.
- **Units in `<home>/.config/systemd/user`, whatever the shell's `XDG_CONFIG_HOME`.** `Site` stays `{UserHome}`. The user manager searches `$XDG_CONFIG_HOME/systemd/user` in place of `~/.config/systemd/user` only when its *own* environment has the variable (`systemd.unit(5)`, "Unit File Load Path"), and it computes its search path from its own process environment, the one PID 1 and the login's PAM session gave it: `environment.d` files and `systemctl --user set-environment` change only the environment its services get (`manager.c`: `manager_run_environment_generators` fills `transient_environment`, and at start `lookup_paths_init` runs before it; `path-lookup.c` reads `getenv`, v240 to main). A shell's `export XDG_CONFIG_HOME=...`, the usual place for it, never reaches the manager, so following the shell's variable would put the units where the manager does not look. The adapter therefore takes no environment: `UnitDir` is `<UserHome>/.config/systemd/user`, so a sandboxed `HOME` or a test's temporary home keeps its units under that home, and `Locate` is its plain inverse. The case this misses is a manager that does have another `XDG_CONFIG_HOME` (set by `pam_env` or a `user@.service` drop-in): `enable --now` fails with "Unit file <ref>.timer does not exist" and setup rolls back. 5b-2 can make this exact by asking the manager (`systemctl --user show --property=UnitPath`) and naming the directory in the failure; until then `Load` adds to that error where the units are and that a manager whose own environment sets `XDG_CONFIG_HOME` does not look there. `schedulertest` gained an optional `Sited` method on a `Manager`, so a fake `systemctl` (which finds units by name, not by the path `launchctl bootstrap` is given) learns the run's user home.
- **Programs systemd refuses.** `string_is_safe` in `load-fragment.c` refuses a program path (after specifiers, however it is quoted) with a control character, DEL, `"`, `'` or `\`, v240 to main, and from v261 also `*`, `?`, `[`, invalid UTF-8 or an empty path. `Plan` refuses all of them on every version, so an upgrade to v261 cannot break a unit it wrote, and its error says to install agent-archive somewhere else (`AGENT_ARCHIVE_INSTALL_DIR`): `install.sh` falls back to `~/.local/bin`, so a home such as `/home/o'brien` needs another directory for the program, while the data directory can stay under it (`Environment=` quotes it and `StandardOutput=append:` takes it as it is).
- **Unit files.** The service is a oneshot: `ExecStart=<exe> _collect`, `Environment=` for `AGENT_ARCHIVE_HOME` and each variable, `StandardOutput=append:<data>/collector.log`, `StandardError=append:<data>/collector-error.log`, no sandboxing. The timer is `OnBootSec=60s`, `OnUnitActiveSec=60s`, `AccuracySec=1s`, `Unit=<ref>.service`, `WantedBy=timers.target`, no `Persistent=`. Quoting: `%` is doubled everywhere; `$` is doubled in `ExecStart=` and left alone in `Environment=` (which does not expand); a word with more than `[A-Za-z0-9_-./:@+,=]` is double-quoted with `\` and `"` escaped; a `StandardOutput=append:` path is written as it is with `%` doubled (systemd takes the rest of the line as the path). `Plan` refuses what a unit file cannot hold or read back as written: a `$` in the program's path (systemd expands argv[0] but not the path), a quote, a backslash, `*`, `?` or `[` in it (systemd refuses such a program however it is quoted; see below), control characters, invalid UTF-8, and an environment name that is not `[A-Za-z_][A-Za-z0-9_]*`.
- **`Definition`** reads the unit files (the service's `ExecStart=` program and `Environment=`), never `systemctl show`. A timer with no service is defined with a `DefinitionErr`; a service with no timer is defined and readable.
- **`Inspect`** asks `systemctl --version` (older than 240: `unknown` and a `Problem` whose fix says to upgrade; RHEL 8's 239 from package release `239-32`, RHEL 8.3, has `StandardOutput=append:` backported and is accepted) and `systemctl --user show <ref>.timer <ref>.service` in one 3 s budget, and, for a job that is active, `loginctl show-user` for lingering. The state map as built: a unit `masked` or loaded with a state systemd rejects, loaded from no file, or still running after its file was deleted and the manager reloaded (`not-found` and active, which nothing ties to an installation), is `unknown`; a unit loaded from a file that is not this site's (`local.SameLocation` on `FragmentPath`) is `another_installation` (`LoadedFrom` and `Expected` name the two files); a service `activating`, `active`, `reloading` or `deactivating` is `running` whatever the timer is doing (a stop must reach it); a timer `active` (or `activating`, `reloading`) is `loaded`, so a failed service under an active timer is `loaded`; anything else, `not-found` and `inactive` included, is `missing`. No user bus, systemd not the init system, or no `systemctl` is `unknown` with a `Problem` whose fix names a login session and `loginctl enable-linger`. `Degraded` holds a drop-in override (`DropInPaths`, except a drop-in for every unit of the type, in a `service.d` or `timer.d` directory, which Fedora ships for every user service) and lingering off.
- **`Load`** is `daemon-reload` then `enable --now <ref>.timer`; **`Unload`** checks ownership as launchd's does, then `disable --now` the timer (when it is loaded), `stop` the service and `reset-failed` it (when it is loaded; a stopped run leaves it `failed`, which keeps the whole user manager `degraded`; a refusal is ignored), and `daemon-reload`. Deleting the unit files afterwards (shared code) needs no second reload: the manager unloads an inactive unit on its own, so a deleted job reads `not-found` (seen on 239, 245, 252 and 255), and `Load` reloads before it enables. Both run on `WithoutCancel` and a 30 s bound; `Inspect`'s questions are the caller's to cancel. `Installed` is the installation's own job alone.
- **`DefaultPATH`** is systemd's compiled-in default (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`), from `systemd.exec(5)`, unverified against a live user manager (which gives its services its own build's user `PATH`, by default this value, not the login's; a distribution or an `environment.d` file may set another); 5c revisits it.
- **Fixtures** for `systemctl show` and `--version` are captured from real user managers of systemd 239 (Rocky 8), 245 (Ubuntu 20.04), 252 (Debian 12) and 255 (Ubuntu 24.04) in disposable containers (their README says how), and the conformance suite runs over each. The same containers ran the adapter itself end to end: the planned units pass `systemd-analyze --user verify`, `Load` starts them, the logs append, the environment's quoting survives `%`, `$`, spaces, quotes and backslashes, and `Unload` leaves nothing running. `deadcode` reports the package as unreachable until `host` constructs it, so `test.yml` excepted `internal/scheduler/systemd` until 5b-2 had `host` construct it (the exception is gone).

## 5b-2 as built

5b-2 wires the systemd adapter into `host`, `setup`, `status`, `refresh`, recovery and `uninstall`, in three stacked pull requests (host selection and the recorded backend; setup, status and refresh wiring; the uninstall flag and the real-manager CI job). Where it settled what the sections above left open:

- **`host`.** `New(system, run)` is launchd on darwin, `systemd.Scheduler` on Linux, and, for an unknown system, `host.Unavailable("none", ...)`: a scheduler whose own name is `none` (a journal it could write is never mistaken for launchd's), that defines no job, reports every job `unknown` with a `Problem` whose fix is the reason, and refuses `Load` and `Unload`. `Named(system, name, run)` is what a recorded name means: the empty name, and the system's own scheduler's name (`none` included), is the system's own, a name whose manager this system does not have (launchd on Linux) or that this build does not know is an error, and `Lookup(name)` is `Named` for this system over `Exec`. `Default` is gone (nothing called it once `cli` looked backends up by name). The default `Exec` runs `systemctl` and `loginctl` with the process's environment, so `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` reach the user manager, plus `SYSTEMD_COLORS=0` so a colored `--version` line still parses; every other program, `launchctl` included, runs with the environment unchanged.
- **`background_backend`.** One optional field in `config.json`. Setup writes `host.Recorded(name)`: the name of the backend it used, except that launchd is left out, so a macOS `config.json` is byte for byte what it was (an absent field means launchd on macOS and systemd on Linux, for all time); a Linux setup writes `"systemd"`. Setup uses this system's own scheduler, whatever was recorded (`Env.choosingBackend`): a recorded backend this system cannot use belongs to another system's manager, so there is no job of it here to retire, and the record is overwritten. Every other command resolves the scheduler from the record when `Env.Scheduler` is nil (`Env.recordedBackend`, read when asked, since setup and uninstall keep the field as they rewrite the file), never re-picks, and gets `host.Unavailable` under the recorded name when this system cannot use it, so `status` says the job is `unknown` and `uninstall` refuses. Recovery resolves the journal's own `backend` by name (`Env.backends` over `newScheduler`), so a journal that names a scheduler this system cannot use is refused with its jobs untouched, in the words it always had. A build from before the field reads a configuration with it (plain `json.Unmarshal`; pinned by a test). A data directory copied from a Mac to Linux records nothing, which there means systemd: its launchd job stayed on the Mac, so `status` finds no job here and setup defines a systemd one. A journal is different: it always names its backend, and an absent one means launchd on any system (every journal without it is macOS's), so a macOS journal copied along is refused rather than recovered through systemd.
- **Deadcode.** `internal/scheduler/systemd` has a production caller now, and the temporary `test.yml` exception for it is gone.

## 5c as built

5c is the Linux job environment and the live acceptance run, in stacked or independent pull requests (environment, snapshot root and `DefaultPATH`; the enable link and the clone warning; the acceptance procedure). Where it settled what the sections above left open:

- **Environment forwarding.** `buildCollectorEnvironment` takes the operating system from its source (`operatingSystem()`): on Linux, for every provider, it records `XDG_CONFIG_HOME` and `XDG_CACHE_HOME` when they are set to an absolute path (cleaned; a relative, empty or `~` value is invalid to the XDG specification and is left out, as `Locations` leaves it out), and an R2 job with neither still gets no environment at all. On macOS and any other system it records neither, so the plists are unchanged (the 5a-0 goldens and their fixtures did not change). `status` compares each with the shell's (`Env.xdgDrift`, next to `awsFilesDrift`, both inside `readBackground`): an unset, relative or empty variable counts as unset, so a shell that sets one the job lacks, sets another, or dropped it is warned, in words that name the variable, both values, and `agent-archive setup` as the fix. Only `status` warns: setup records the shell's values itself, and `setup --refresh` keeps the recorded environment as it does the AWS variables (it says nothing about drift today either).
- **Snapshot root.** `Locations.SnapshotRoot` on Linux is `<cache>/agent-archive/cursor-snapshots`, where `<cache>` is an absolute `$XDG_CACHE_HOME`, else `.cache` in the account's home from the user database (`LocationDeps.AccountHome`, read by `cursorstore` with `os/user`, never `$HOME`); with neither it is "" and no snapshot is taken (the same fail-closed answer as an unknown system). `Locations.SnapshotCacheDir` is `<cache>/agent-archive`. `cursorstore` makes the cache home (0700, as the XDG specification says, only when it was missing) and that folder, refuses it unless it is a real directory of the user's that no one else can write to (mode without `0o022`, owned by the user, not a link: a directory others can write to could have the 0700 root inside it swapped after the checks), and writes a `CACHEDIR.TAG` there (the signature line and comments the Cache Directory Tagging Specification gives) once, leaving any existing file alone and ignoring a failure to write it (a copy is removed as soon as it is read). The root itself keeps the existing 0700, ownership and link checks. Nothing is migrated: Linux never shipped, and a `/tmp/agent-archive-cursor-<uid>` left by a development build is abandoned, not swept. macOS is unchanged, `SnapshotDirName` included. Because `XDG_CACHE_HOME` is forwarded, a shell and the job agree on the root and the stale-copy sweep sees one directory.
- **`DefaultPATH`.** Read from a live user manager (Ubuntu 24.04, systemd 255): a service started by `systemd-run --user` gets the manager's own PATH, `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/games:/usr/local/games:/snap/bin` (Ubuntu's `/etc/environment` adds the last three to the built-in value), never the login shell's, so no directory under the home is on it. The constant is kept: it is the built-in value, which every manager gives at least. The composition is right for helpers under the home (aws-vault, 1Password's `op`, pipx in `~/.local/bin`) because setup records this shell's usable PATH entries first and `DefaultPATH` after them (`collectorPath`); a test pins it with the real value. The known limit, as on macOS: a directory that does not exist when setup runs is not recorded, so a helper installed there later is found only after setup runs again (`status` reports a `credential_process` the collector cannot find).

## Definition of done for "platform-agnostic"

Everything except `internal/scheduler/host` and the adapter directories compiles and passes its tests against `internal/scheduler` and `internal/platform` alone, using the schedulertest model. Adding an operating system or a second scheduler touches only its own adapter directory, `host`, the OS value, and the docs.
