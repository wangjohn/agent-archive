# Architecture

agent-archive is one Go binary, `cmd/agent-archive`, with everything else in
`internal/`. The design rationale is in the [archive design](../specs/archive.md);
this page is the map.

```mermaid
flowchart LR
  subgraph apps[Coding agents]
    CC[Claude Code] & CX[Codex] & CU[Cursor]
  end
  apps -- lifecycle hook --> H["agent-archive _hook<br/>(fast, no network)"]
  H --> LS[(Local state<br/>~/.local/share/agent-archive)]
  L[LaunchAgent, every 60 s] --> C["agent-archive _collect<br/>collector + retention"]
  LS --> C
  T[(Transcripts<br/>and Cursor's DB)] --> C
  C -- "filter → bundle → upload → read back" --> B[(Your S3 / R2 bucket)]
  B --> R["list · show · handoff<br/>(reader)"]
  T --> BF["backfill<br/>(plan, register)"] --> LS
```

## The paths through it

1. **Hook** (`internal/capture`, run by `_hook`). An app runs `agent-archive _hook
   --harness <app>` at session start, stop, and similar events. The hook checks the
   configuration, records the session (`registrations/`) or queues work
   (`requests/`) under `hooks.lock`, and returns within milliseconds. It does
   no filtering or uploading, never touches the network, and never writes to
   stdout. The `_hook` command in `internal/cli` is only the adapter: it
   parses `--harness`, decodes stdin, and always exits 0, recovering a panic
   into a diagnostic.
2. **Collector** (`internal/collector`, run by `_collect` or `sync`). Under
   `collector.lock`, for each registered session it reads the transcript,
   filters it through the app's adapter, builds a source bundle, derives
   metadata, and publishes source then metadata, verifying the source in
   storage (its SHA-256 and size, or a read-back where the service reports
   no checksum) before the metadata points at it. Retention runs after each
   pass.
3. **Reader** (`internal/reader`). `list`, `show`, and `handoff` read the
   bucket: metadata first, and a source only when asked, verified against
   the metadata's checksum.
4. **Backfill** (`internal/backfill`). Plans an import of existing
   transcripts read-only, then registers the confirmed sessions in short holds
   of `hooks.lock`; the collector uploads them like any other.

## Packages

| Package | Owns |
| --- | --- |
| `archive` | The privacy filter and adapters (one per app), source bundles, metadata derivation, the normalized view, and handoff rendering. No filesystem, network, or CLI dependencies, so it is fully testable on fixtures. |
| `collector` | The scan, build, publish loop; change detection; subagent capture. |
| `state` | Per-session local state: registrations, requests, published and pending publications, change detection, removal records, and the per-session locks (`Store`; see [local state](../../docs/reference/local-state.md)). `state/statetest` has test helpers. |
| `retention` | Deleting superseded snapshots and expired sessions, with the remote metadata as the source of truth. |
| `storage` | The object-store contract and the S3/R2 implementation; checksums, read-back, bucket privacy inspection, and `Diagnose`, which names a storage failure's cause in plain words. Keys are relative to the configured prefix. `storage/storagetest` has the in-memory store tests use. |
| `credentials` | Resolving storage credentials: AWS profiles, and R2 secrets in the Keychain (cgo, Security.framework). |
| `config` | `config.json`: the one record of how this Mac is set up. |
| `local` | The data directory, atomic durable writes, and file locks. |
| `platform` | The one place that knows the operating system: `platform.OS` (`Darwin`, `Linux`, and an `Unknown` that fails closed), `platform.Current` (the only non-test reader of `runtime.GOOS`, `TestOnlyPlatformReadsRuntimeGOOS`), and `Locations`, where the OS keeps Cursor's data, the desktop apps' folders, temporary directories, privacy-protected folders and the Cursor snapshot root. Pure: it runs no program and opens no file (the two inputs that need the system come in as `LocationDeps`), and imports no `credentials` or `hooks` (depguard and `TestPlatformImportBoundary`). |
| `hooks` | Checking, planning, installing, and removing the hook entries in the apps' hook files. |
| `scheduler` | The port to whatever runs the background collector: `Ref`, `Site`, `JobState` (the words `status` reports), the desired state of a job (`Installation`, `JobSpec`, and `Plan`, a pure rendering of a definition as file `Artifact`s), what a scheduler reports (`Status`, with the `Problem` facts and `Words` nouns the commands' messages are filled from), the `Scheduler` interface (`Definer`, `Inspector`, whose `Installed` lists an installation's own job and its aliases, and `Controller`), `Retiree`, the job setup retires, the `NotOwnedError` and `IndeterminateError` an `Unload` refuses with, and the `Runner` an adapter runs its tool through. Pure: it runs no program and imports no `credentials` or `hooks` (depguard and `TestSchedulerImportBoundary`). |
| `scheduler/launchd` | The macOS adapter: the LaunchAgent plist (planning it and reading its program, environment and data directory back), the collector's labels and every earlier one (`Installed`: the labels earlier releases gave the collector, and the prototype's upload job), and the `launchctl` calls that ask about, load and stop a job, all through the `Runner` it is given. |
| `testutil/schedulertest` | Test helpers for the port: `Model`, a scheduler that is not launchd, and `RunConformance`, the suite every adapter passes over a fake `Runner`. |
| `scheduler/host` | Chooses the adapter for the system (launchd on macOS; on another system, a scheduler that says every job is unknown and refuses changes, until it has one) and owns the real `Runner`: the program in a process group of its own, output combined. Only `host` (and `cli`, for launchd's vocabulary, until it moves behind the adapter) imports an adapter (depguard and `TestOnlyHostImportsAdapters`); running a program is limited to a listed set of packages (`TestOnlyListedPackagesRunPrograms`). |
| `agentskills` | The skills setup installs into Claude Code, Codex, and Cursor (`/handoff`): the `Registry`, each skill's text as an embedded template rendered per destination, and planning their install, removal, and refresh as setup-journal changes. A file is setup's by a marker line and this installation's data directory; `Stale` names the files an upgrade has outdated. |
| `capture` | The hook runtime: classifying a hook event, admitting a new session or continuing a registered one, lifecycle and final-response evidence, subagent links, and the content-free capture diagnostics status shows. It imports nothing from the command line, even transitively, and does not itself import anything that runs a program, opens the Keychain, or uses the network (`credentials` and `storage` come in only through `config`, for their types); depguard and `TestCaptureImportBoundary` enforce this. |
| `gitremote` | Where the program runs `git` to ask a name of it (handoff's `--worktree` runs it for its own changes): reads a project's `origin` with a 500 ms timeout and returns `archive.RepoKey` of it, and reads the branch checked out in a directory, each "" on any failure. `capture` may not run programs, so `cli` hands the hook this lookup (`capture.WithRepoKey`). |
| `setupjournal` | Setup's transaction (`setup-transaction.json`): writing the journal before any hook file or the LaunchAgent changes, rolling a failed setup back, recovering an interrupted one without overwriting later edits, and recording the prototype's job and the collectors installed under earlier labels that setup retires (which jobs those are is the scheduler's to say). launchd is reached only through the `Launchd` its caller passes (cli's `Env`). Every command and the hook check whether a journal is pending. |
| `evidence` | Skill inventories and snapshots, as privacy-filtered evidence. |
| `cursorstore` | Reading Cursor's `state.vscdb` without writing to it or beside it. |
| `backfill` | Discovery, the import plan, registration, and undo. |
| `reader` | Listing metadata and loading verified sources, with a disposable metadata cache. |
| `cli` | Every command: flags, prompts, rendering, and the wiring between packages. Terminal output takes its colors, symbols, wrapping, and spinner from `ui.go`, which prints plain text when output is not a terminal, `NO_COLOR` is set, or `TERM` is `dumb`. The `stats` screens are pure page functions (`renderPage` in `stats_render.go`: numbers and a view in, lines out) with one color table (`stats_colors.go`). Process state (args, stdio, the clock, the home directory, launchctl, the Keychain) reaches commands through an injectable `Env`. A few lower packages still read the process directly: `local` (`AGENT_ARCHIVE_HOME` and `$HOME`), `credentials` (AWS configuration files and the Keychain), and `cursorstore` (the user's temporary directory, through `getconf` on macOS). |
| `statsfmt` | How `stats` writes a number: token counts (`4.9M`), money, percentages, ordinals. Shared by the terminal screen and the page so one number reads the same on both; pure, imports only `archive` (depguard and `TestFormattersImportBoundary`). |
| `statshtml` | Renders the engine's `stats.Stats` as one self-contained HTML page for `stats --html`: an embedded `html/template` and stylesheet, inline SVG charts (daily bars, a token-composition donut), no script and no external request, valid as strict XML, light, dark, print and forced-colors. Pure: it imports only `stats` and `archive` (depguard and `TestRendererImportBoundary`), reads no clock, and every name reaches the page as a `string` that `archive.DisplayLine` has cleaned and the template has escaped (`TestNothingIsMarkedTrusted` forbids the `template` types that skip escaping). Project, skill and MCP server names are replaced by "project A", "skill A", "MCP server A" unless asked for. |
| `stats` | The numbers behind `stats`: tokens, sessions, estimated cost, agents, models, projects and highlights over a window of days, computed from session metadata by one pure function (`Compute`). It takes the metadata, the time, the time zone and a price table as arguments, so it reads no clock, file, environment or network and imports only `archive`'s types (depguard and `TestStatsImportBoundary` enforce this). Cost comes from a dated, versioned price table embedded in the package (`prices.json`) that a person's own JSON file can override; a model the table does not list is left unpriced, never guessed. |
| `doclinks` | A test that the documentation's relative links resolve. |

## Package dependencies

Arrows point at what a package imports; `local` and `archive`, which nearly
everything imports, are left out. Only `cmd/agent-archive` imports `cli`; `capture` and
`setupjournal` never import `cli`, `terminal`, or `golang.org/x/term`
(depguard in `.golangci.yml`, and a test in each package).

```mermaid
flowchart TD
  cli --> capture & setupjournal & backfill & collector & retention & reader & hooks & evidence & agentskills
  cli --> config & state & storage & credentials & cursorstore & terminal
  capture --> setupjournal & config & state
  setupjournal --> hooks & state
  agentskills --> hooks
  backfill --> collector & retention & config & state & storage & cursorstore & terminal
  retention --> reader & state & storage
  collector --> state & storage & cursorstore
  reader --> storage
  config --> credentials & storage
  state --> cursorstore
  storage --> credentials
```

## Invariants worth knowing before you change anything

- Nothing leaves the Mac except what an adapter's filter kept. Unknown keys
  are dropped and named, never passed through. See
  [privacy](../../docs/security/privacy.md) and [versions](../maintainers/versions.md).
- The bucket's `metadata.json` is the live pointer. A source is uploaded
  before the metadata that points at it, and deleted only after it is no
  longer pointed at.
- Pending publication bytes are frozen before the first remote write, so a
  retry uploads byte-identical objects.
- Local writes are atomic and durable (temporary file, fsync, rename,
  directory fsync). One unreadable state file is quarantined, not fatal.
- Hooks must be fast and silent: an app waits for them. `capture` waits at
  most a second for `hooks.lock` and 50 ms for `diagnostics.lock`, and the
  `_hook` command exits 0 whatever happens.
- Every side effect in `cli` goes through `Env`, so tests never touch the real
  home, launchd, Keychain, or a bucket.
