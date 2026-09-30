# Platform abstraction: one seam for every operating-system difference

> **Proposed.** Not implemented. It supersedes sections 3c to 3e and the "Linux PR list" of [portable-handoff-and-onboarding.md](portable-handoff-and-onboarding.md) for PRs 5 to 7. Linux PRs 1 to 4 (release artifacts, installer, Cursor paths, credential store) are already in review or merged and are not re-planned here, except where this plan folds their OS switches into the new seam.

Status: proposed for review. Prepared 2026-09-30 from an inventory of `origin/main` at `61fb6ea` and of the open Linux PR branches.

## Why this document exists

About 95% of the code is already platform-independent: the capture core, adapters, filtering, storage, retention, reader, stats and most of backfill. The remaining OS-specific code is small, but it is not in one place, and the Linux work so far has spread it further. If PR 5 and 6 add "a systemd path" next to the launchd one the same way, the repository will have two of everything and a growing set of `if goos == ...` branches. The goal here is the opposite: Linux is the second implementation of a small number of ports, and a third operating system is a third implementation, not a third round of edits through `cli`.

### What the inventory found

1. **Launchd is woven through 19 production files.** The job-definition renderer and three parsers live in `internal/hooks/install.go` (the package that edits app hook files). The job identity token is the plist **file path** everywhere (`JobState(plist)`, `Load(plist)`, `Journal.Plist`). `setupjournal` claims to be neutral but its interface, error texts and JSON are launchd-shaped.
2. **Job state is a public stringly-typed vocabulary.** `loaded`, `running`, `missing`, `unknown` and `another_installation` are compared in about ten places and are also the `status --json` `background` wire values.
3. **Six independent `GOOS` switches exist or are about to.** On `main` there is one (`cursorstore` temp dir). Linux PR 3 added `AppSupportDir(goos)`, `backfill.Environment.GOOS`, `Env.BackfillGOOS` and the `capabilities.go` `goos` parameters. Linux PR 4 added `credentialGOOS`, `OpenOptions.GOOS` and `StoreName(goos)`. None goes through a single struct, and `Env.BackfillGOOS` also decides where the collector finds Cursor's database.
4. **macOS history is entangled with the shared flow.** The relabel and legacy-prototype migration (`previousCollectorPlists`, `PlanRelabel`, `PlanLegacyMigration`, `LegacyJob`, `Relabeled` in the journal) exists because of pre-v0.1.0 development builds. A Linux backend never had earlier labels, yet the shared transaction code and the journal know about them.
5. **Test fixtures encode the launchd shape.** 19 test files set `LoadLaunchAgent`, `UnloadLaunchAgent` or `JobState` directly, 39 mention launchd vocabulary, and 61 of 129 `cli` test files inherit the stand-ins through `testEnv`.

## Goals and non-goals

Goals:

- One package (`internal/platform`) holds the vocabulary and the ports; one composition root decides which implementation runs. **No `runtime.GOOS` outside that root**, enforced by a test.
- Adding an operating system, or a second scheduler on the same OS (cron, a hook-triggered fallback), means writing an adapter and passing a shared conformance suite. Nothing in `setup`, `status`, `uninstall`, backfill or the collector changes.
- macOS behavior is byte-identical: existing plists, Keychain items, journals, `status --json` values and every macOS string.
- Tests can model any platform on any host through the same ports, instead of pinning `GOOS` strings.

Non-goals: Windows (the code is Unix-only through `flock`, `syscall.Kill` and signals; this plan makes that explicit with `//go:build unix`), local-only or folder backends, `wget` installs, libsecret, journald logging, and implementing a cron or hook-triggered fallback (the design must allow one, not build it).

## Design

### Package layout and import rules

```
internal/platform/            vocabulary + ports (interfaces, value types). No os/exec, no runtime.GOOS.
internal/platform/launchd/    scheduler adapter: plist codec, launchctl calls, macOS legacy migration.
internal/platform/systemd/    scheduler adapter (PR 5b).
internal/platform/host/       composition root: New(goos, deps) platform.Platform. The only reader of runtime.GOOS.
internal/platform/platformtest/  fakes, the scheduler conformance suite, model platforms for tests.
```

- `internal/cli` and `cmd` import `host`. Everything else imports at most `platform` (types and ports).
- `platform` is pure so that packages with import-boundary tests (`capture` and `setupjournal` forbid `os/exec`) can accept its interfaces. Adapters shell out only through an injected `Runner`, never `os/exec` directly outside `host`'s default runner.
- `hooks` goes back to editing hook files only. The plist renderer and parsers move to `platform/launchd` (pure code motion, first PR).

### The `Platform` value

```go
type Platform struct {
    Scheduler   Scheduler   // background collector job
    Credentials Credentials // R2 secret store and its user-facing name
    Locations   Locations   // app and OS paths
    Apps        Apps        // which agent apps are installed and at what version
    Terminals   Terminals   // open a command in a new terminal (handoff)
    Files       Files       // file birth time and other per-OS file facts
}
```

`Platform` lives in `Env` (one field replaces `JobState`, `LoadLaunchAgent`, `UnloadLaunchAgent`, `Keychain`, `BackfillGOOS`, `DiscoverApplications` and the `credentialGOOS` variable). It is a bundle for the composition root and `Env`, **not a service locator**: each `cli` file declares the narrow interface it needs (the pattern `preflightDependencies` already uses) and receives it explicitly, so a function's dependency on "the scheduler" or "the credential store" stays visible in its signature.

### Scheduler port

The port is defined by what the application needs, in domain terms, not by launchd verbs.

```go
// JobKey names a collector installation. Empty for the default installation,
// otherwise the first 12 hex digits of sha256 of the canonical data directory:
// exactly the digest CollectorLabel computes today, so macOS labels do not change.
type JobKey string

type JobSpec struct {
    Key        JobKey
    Executable string            // absolute
    Args       []string          // ["_collect"]
    DataHome   string            // absolute; exported to the job as AGENT_ARCHIVE_HOME
    Env        map[string]string // built by collector_env.go; never carries credentials
    Interval   time.Duration     // 60s
    RunAtLoad  bool
    StdoutPath string            // <data home>/collector.log
    StderrPath string            // <data home>/collector-error.log
}

type JobState string // "loaded" | "running" | "missing" | "unknown" | "another_installation": the status --json wire values, now typed

type Scheduler interface {
    // Name is the backend's stable identifier ("launchd", "systemd"), persisted in config and journals.
    Name() string
    Terms() Terms                       // user-facing nouns and commands ("LaunchAgent", "launchctl print gui/$(id -u)")
    Available(ctx context.Context) Availability // {OK, Reason, Fix}: no user bus, no launchd session
    DefaultPATH() string                // the PATH the job gets when the definition sets none

    Define(spec JobSpec) (Definition, error)    // PURE. The files to write; same input, same bytes.
    Inspect(ctx context.Context, key JobKey) (JobInfo, error)
    Load(ctx context.Context, def Definition) error
    Unload(ctx context.Context, key JobKey) error  // ownership-checked; nil when not loaded
    Supersessions(ctx context.Context, key JobKey, dataHome string) ([]Supersession, error) // earlier jobs to retire; nil off macOS
}

type Definition struct {
    Key   JobKey
    Files []FileChange // one file for launchd (plist), two for systemd (service + timer)
}

type JobInfo struct {
    State           JobState
    Program         string            // recovered from the definition, for "is the binary still there"
    Env             map[string]string // recovered, for the drift checks status runs
    DataHome        string
    DefinitionPaths []string          // what uninstall removes
}
```

Contract points that were implicit before and become explicit:

- **Ownership belongs to the backend.** `Inspect` returns `another_installation` when the platform's registered definition is not ours (launchd: the `path =` line of `launchctl print`; systemd: `FragmentPath`, both compared with `local.SameLocation`), and `Unload` stops nothing in that case. Shared code never compares paths or labels.
- **The identity token is `JobKey`, not a file path.** A backend whose job is two files, or none, is representable. `launchLabel(plist)` (label recovered by stripping a file name) disappears from `cli`.
- **`Define` is pure and `Inspect` round-trips it.** The conformance suite asserts that `Inspect` over what `Define` produced returns the same program, environment and data home.
- **Available replaces GOOS gates.** `setup` refuses, with a fix, when `Available` is false. This is the resolution of the cloud-capture requirement that `setup` be gated on "a working scheduler exists", not on `runtime.GOOS`.
- **Job environment is a scheduler fact.** `DefaultPATH()` replaces the `launchdPath` constant used in two places (`collectorPath` at setup time and `collectorEnvironmentProblems` at status time).

**Supersessions.** The macOS-history machinery (label relabeling after the pre-v0.1.0 builds, the `com.agent-skills.skill-runs-upload` prototype job) stays, but moves entirely behind `Supersessions` in the launchd adapter. It returns generic values (`Ref`, file changes to make, was-loaded); the shared transaction retires and, on failure, restores them without knowing why they exist. `isCollectorLabel`, `previousCollectorPlists`, `PlanLegacyMigration` and `PlanRelabel` leave `cli` and `setupjournal`. The Linux backend returns nil.

**Which backend runs.** `host.New` picks the platform default (launchd on darwin; on Linux, systemd) and setup **records the chosen backend name** in `config.json` (`background_backend`; absent means "the platform default at the time", which is launchd for every existing macOS install). Status, uninstall and recovery use the recorded backend even if it is no longer `Available`, so a later change (lingering disabled, a fallback added) cannot orphan an installed job.

### Credentials port

`credentials.CredentialStore` and `R2Credentials` are frozen (the guided-R2 work depends on them). The port adds only what wording and setup need:

```go
type Credentials struct {
    Open func() (credentials.CredentialStore, error)
    Info StoreInfo // Name ("Keychain" / "credentials file"), recovery hints, whether the store can hold a secret across processes
}
```

`credentialGOOS`, `credentials.StoreName(goos)`, `UsesKeychain(goos)` and the `goos string` parameter on every `credential_words.go` function are replaced by `StoreInfo`. The Linux file and environment stores stay in `credentials`; only the choice moves to `host`. A future libsecret store is an adapter.

### Locations, Apps and Files ports

```go
type Locations interface {
    CursorStateDB() string
    CursorWorkspaceStorage() string
    ClaudeDesktopScratch() (string, bool)   // false: not a thing on this OS
    CodexDesktopDocuments() (string, bool)
    TempRoots() []string                    // backfill skips these
    PrivacyProtected() []string             // TCC on macOS, none on Linux
    SnapshotRoot() string                   // Cursor database copies (see open decision O2)
}
type Apps interface { Discover(ctx context.Context) Applications } // Codex, Claude, Cursor: installed, version, and "unknown" distinct from "absent"
type Files interface { Birth(info fs.FileInfo) (time.Time, BirthSource) } // BirthSource: created | modified
```

- These replace `AppSupportDir(home, getenv, goos)`, `backfill.Environment.GOOS`, `Env.BackfillGOOS`, `defaultTempDirs(goos)`, `isMac()`, the `discoverApplicationsFor(goos)` parameters and `userTempDir`'s hard-coded `runtime.GOOS`.
- `BirthSource` also fixes a provenance wart from the Linux PR 3 review: on Linux `started_at_source` is `file_created` while the value is an mtime. A `modified` source lets archive consumers tell the two apart (a metadata change, so it follows the metadata versioning rules).
- Hook config paths (`~/.claude`, `~/.codex`, `~/.cursor`) are the same on every OS and stay where they are.

### Terminals port

`internal/termlaunch` (handoff v2, in review) already has an injected `Environment{GOOS, LookupEnv, Run}` and a darwin-only osascript path with tmux. It becomes the darwin and tmux adapters of a `Terminals` port; the linux adapter starts as tmux plus "print the command", which is what it does today, and gnome-terminal or konsole can be added later without touching handoff. This is a follow-up to that PR, not a change to it.

### Wording

Platform nouns come from the adapters, not from strings scattered in `cli`: `Scheduler.Terms()` (manager, job noun, how to inspect it), `StoreInfo.Name`. The plain "Mac" in neutral prose is a separate copy pass (PR 7), not part of this seam. The preflight label "Background job" and its launchctl-specific detail become `Terms`-driven.

### Persisted formats and compatibility

| Item | Rule |
| --- | --- |
| `setup-transaction.json` | Transient, but recovery after an upgrade mid-setup must work. The reader accepts the current shape (`plist`, `legacy`, `relabeled`, `more_relabeled`, `changes`, `was_loaded`) and converts it. The writer adds a backend-tagged `job` object and superseded list; the launchd backend keeps writing `plist` until the compatibility window (one minor release after this ships) closes. Fixture journals are generated from the pre-refactor code and replayed by a test. |
| `status --json` `background` | Values unchanged (`JobState` constants equal the wire strings). |
| Plist bytes, label, file location, log paths | Byte-identical, pinned by golden files created **before** any code moves. |
| `_collect` | Unchanged; installed jobs depend on it. |
| Keychain service and JSON tags | Unchanged. |
| `config.json` | One optional new field (`background_backend`), absent for existing installs. |

## Behavior-preservation controls

1. **Characterization first.** A tests-only PR pins today's macOS behavior before any move: golden plists for the default and a non-default installation, transcripts of `setup`, `status`, `status --json` and `uninstall` against the stub launchctl, fixture journals from v0.1.x code (including a relabel and a legacy-migration case), and `parseJobState` over recorded `launchctl print` output.
2. **Architecture tests that hold the line** (added in the seam PR, exempting only the listed adapters):
   - no `runtime.GOOS` outside `internal/platform/host` and `cmd`;
   - no `launchctl`, `plist`, `LaunchAgent`, `systemctl` or `Keychain` identifiers outside adapters and the wording tables they feed;
   - no `os/exec` outside the adapters, `host`'s default runner, the pager and the existing listed importers;
   - `platform` imports no adapter.
3. **A shared conformance suite** (`platformtest.RunSchedulerSuite`) that every scheduler backend passes over a fake `Runner`: the five-state matrix, ownership refusal, idempotent unload, `Define` purity and golden output, `Define`/`Inspect` round trip, no credential values in a definition, and the `Available` reasons. A third backend is done when it passes the suite.
4. **Model platforms in tests.** `platformtest.Model("darwin")` and `Model("linux")` build a real `Platform` from the real adapters over a fake `Runner` and an in-memory filesystem. Harnesses stop pinning `GOOS` strings; a test that needs "a Mac" asks for the model. Fail-closed stand-ins (the `runLaunchctl` panic in `isolation_test.go`) become the default `Runner` of `testEnv`.

## Work plan

Every PR follows the standing process: a separate implementer (worktree, fresh context), a separate reviewer who mutation-tests, a second review after any fix round that touches correctness, `main` merged in before review, and CI (including the Levenshtein lint and both deadcode passes) green on the final head. Implementers must run the pinned Levenshtein lint locally. Nothing merges without the owner's say-so, except docs-only plan PRs.

| PR | Scope | Depends on | Owns |
| --- | --- | --- | --- |
| **P0** | This document | none | `dev/proposals/platform-abstraction.md`, a pointer in the older plan |
| **5a-0** | Characterization: goldens, transcripts, fixture journals (tests only) | none | new tests and testdata in `internal/hooks`, `internal/cli`, `internal/setupjournal` |
| **5a-1** | Move the plist codec and label functions out of `hooks` into `platform/launchd` (pure code motion, tests move too) | 5a-0 | `internal/hooks/install.go` (delete), `internal/platform/launchd/` |
| **5a-2** | `platform` types and the `Scheduler` port; launchd adapter over an injected `Runner`; `host` root; `Env.Platform` replaces `JobState`, `LoadLaunchAgent`, `UnloadLaunchAgent`; typed `JobState`; `Terms`; rewrite the 19 directly coupled test files onto `platformtest`; conformance suite (launchd passes it); architecture tests for scheduler identifiers | 5a-1 | `internal/platform/**`, `internal/cli/{env_defaults,install_paths,collector_env,status,uninstall,setup_preflight,setup_transaction,cli}.go`, `setupjournal` interface types |
| **5a-3** | Generalize the journal and supersession: `job` object, `Supersessions`, compatibility reader, macOS history moved fully into the launchd adapter | 5a-2 | `internal/setupjournal/**`, the launchd adapter's legacy code, `install_paths.go` legacy helpers |
| **5a-4** | Fold the other OS switches into `Platform`: `Credentials`/`StoreInfo`, `Locations`, `Apps`, `Files`; delete `credentialGOOS`, `Env.BackfillGOOS`, `Environment.GOOS`, the `goos` parameters; `BirthSource`; `//go:build unix` on `flock` users | Linux PRs 3 and 4 merged; can run in parallel with 5a-2 and 5a-3 on disjoint files, then rebase | `internal/credentials` wiring, `internal/cli/{credential_words,capabilities,backfill}.go`, `internal/backfill/**`, `internal/cursorstore/**` |
| **5b** | systemd user backend: unit and timer rendering, `Inspect` via `systemctl --user show`, `Available` (no user bus, lingering), conformance suite pass, `host` selection, `background_backend` in config, preflight through `Terms` | 5a-2, 5a-3 | `internal/platform/systemd/`, `host`, config field, docs of the systemd behavior |
| **5c** | Linux job environment and end-to-end: `DefaultPATH`, forward `XDG_CONFIG_HOME` to the job, Cursor snapshot root under a per-user cache directory, machine-id clone warning, a live acceptance run on a real Linux host with systemd (documented in `dev/contributing/testing.md`) | 5b | `collector_env.go` PATH policy, `Locations.SnapshotRoot`, docs |
| **T** | `termlaunch` becomes the `Terminals` port's adapters | handoff v2 termlaunch merged, 5a-2 | `internal/termlaunch`, one call site in handoff |
| **7** | Terminology and "Linux supported" docs, `multiple-macs.md` to `multiple-machines.md` with every inbound link, FAQ, README, `CONTRIBUTING.md`, `dev/specs/archive.md` scope | 5b, 5c live-verified | docs, help strings, goldens |

Fixed names (so parallel packages do not collide): `platform.Platform`, `platform.Scheduler`, `platform.JobKey`, `platform.JobSpec`, `platform.JobState` and its five constants, `platform.Definition`, `platform.JobInfo`, `platform.Supersession`, `platform.Terms`, `platform.Availability`, `platform.Runner`, `host.New`, `platformtest.Model`, `platformtest.RunSchedulerSuite`.

## Open decisions for the owner

| # | Decision | Recommendation |
| --- | --- | --- |
| O1 | Persist the chosen scheduler backend in `config.json`? | Yes, so uninstall and status always address the job that setup created. Absent means launchd on macOS. |
| O2 | Where do Cursor database snapshots live on Linux? The macOS location uses the per-user temp directory; on Linux `/tmp` is shared, so the root name is predictable and another local user can block it, and a scheduled job does not inherit the shell's `TMPDIR`. | Per-user, disk-backed cache directory (`$XDG_CACHE_HOME`, default `~/.cache`, under `agent-archive/cursor-snapshots`, 0700). macOS unchanged. |
| O3 | How long does the journal keep writing the legacy `plist` key? | One minor release after 5a-3 ships, then stop. It only affects recovery after an interrupted setup across an upgrade. |
| O4 | Forward `XDG_CONFIG_HOME` to the job, or record the resolved Cursor database path at setup? | Forward it (the same pattern as the AWS variables), so backfill and the collector cannot disagree. |
| O5 | Is `BirthSource: modified` a metadata schema bump now or later? | Now, in 5a-4, since Linux will emit it; follow `dev/maintainers/versions.md`. |

## Risks

- **The port is too launchd-shaped.** Mitigated by designing `Define`, `Inspect` and `JobKey` against both backends before 5a-2 is written; the systemd adapter's needs (two files, `FragmentPath`, user bus) are the design check, and the reviewer of this document should try to break the port with them.
- **5a-2 is large.** It moves the seam under 12 production files and 19 test files. Characterization first and a mechanical test migration keep it reviewable; if it is still too large it splits at the `Env.Platform` swap.
- **The seam becomes a service locator.** The narrow-interface rule and the architecture tests are the guard.
- **The journal migration loses a recovery.** Fixture journals from the shipped versions and a replay test are release-blocking.
- **Linux behavior we cannot see from a Mac.** Real systemd behavior (lingering, user bus, `append:` logging on systemd older than 240, the Cursor Linux path, cursor-agent hooks reportedly failing silently) is unverified until 5c's live run; docs do not advertise Linux before that passes.

## Definition of done for "platform-agnostic"

Everything except `internal/platform/host` and the adapter directories compiles and passes its tests against `platform` alone, using the model platforms from `platformtest`. Adding an operating system or a second scheduler touches only its own adapter directory, `host`, and the docs.
