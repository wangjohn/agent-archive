# `agent-archive handoff` — engineering spec

Status: implemented (PRs H1–H3). Written 2026-09-22 from a probe of real
Claude Code, Codex, and Cursor transcripts run through filter 3 / parser
0.6.0. Depends on PR C1 (`fix/prompt-accuracy`, parser 0.7 / filter 4) being
merged first; see [Sequencing](#sequencing).

## Problem

A person switching coding agents mid-task (Codex → Claude Code, Claude Code
→ Cursor, or the same agent on another Mac) has no way to carry the
conversation over. Each app resumes only its own sessions, from its own
local files. Today the closest path is `agent-archive show ID --normalized`,
which prints ~350 KB of JSON for a medium Claude session, omits every tool
result's text, and has no "the session I was just in" selector.

The archive already does the hard part. On the probe:

| Session | Raw transcript | Normalized JSON | Rendered handoff (prototype) |
|---|---|---|---|
| Claude Code, 19 prompts, 117 tool calls | 7.0 MB | 353 KB | 102 KB |
| Codex, 6 prompts, 18 tool calls | 406 KB | 14 KB | 6 KB |
| Cursor, 78 tool calls | 130 KB | 148 KB | 63 KB |

The filter strips injected instructions and credentials, and the three
harnesses normalize to one shape. What is missing is a command that selects
a session, renders it as a prompt another agent can act on, and fits a
context budget.

## Goals

1. One command produces a handoff document for any archived session, from
   any of the three harnesses, readable by any of them.
2. `--latest` finds "the session I was just in" for the current project
   without the user copying an ID.
3. Same machine: works immediately, without waiting for a collector pass or
   an upload, and while collection is paused.
4. Across machines: works from the bucket alone.
5. Output fits a byte budget by eliding the least useful content first,
   never the person's prompts or the last exchanges.
6. Nothing leaves the privacy filter's guarantees: every rendered byte comes
   from filtered records.

## Non-goals

- Converting a transcript into another harness's native, resumable session
  file. That format is private, versioned per app, and fragile to forge.
  The receiving agent starts a new session with the handoff as context.
- Moving or inspecting workspace state. Uncommitted changes stay on the
  source machine, and v1 does not run `git` (see
  [Workspace section](#workspace-section)).
- Summarizing with an LLM. The output is deterministic rendering; the
  receiving agent does its own summarizing.
- Historical import. `agent-archive backfill` imports sessions the archive
  never captured; until then they are only reachable through `--file` on the
  machine that has them.

## Command

```
agent-archive handoff [SESSION_ID | --latest] [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `SESSION_ID` | — | Archive session ID from `list`. Mutually exclusive with `--latest` and `--file`. |
| `--latest` | off | Most recent session for the project (see [Selection](#selection)). |
| `--project DIR` | current directory | Project used by `--latest`. |
| `--harness NAME` | any | Restrict `--latest`, or disambiguate an ID, to `claude`, `codex`, or `cursor`. |
| `--file PATH --harness NAME` | — | Render a native transcript file directly. Same machine only; still filtered. |
| `--source auto\|local\|archive` | `auto` | Where the session content comes from (see [Content source](#content-source)). |
| `--max-bytes N` | `120000` | Output budget in bytes (≈30k tokens). `0` means no limit. When the budget trims anything, the untrimmed version is also saved (see [Full version](#full-version-when-trimmed)). |
| `--format markdown\|json` | `markdown` | `json` emits the structured `Handoff` document before rendering. |
| `--output FILE` | stdout | Write to a file created with mode `0600`. Refuses to overwrite an existing file. |
| `--force` | off | With `--output`, overwrite an existing file. |
| `--no-preamble` | off | Omit the instructions addressed to the receiving agent. |

Exit codes follow `show`: 0 success, 1 runtime failure, 2 usage error. When
setup has never run, the command prints the shared `notSetUpMessage` and
exits 0, except with `--file`, which needs no setup.

Documented recipes (help text and `docs/install.md`):

```sh
# Continue in Claude Code what you started in Codex, same repo
claude "$(agent-archive handoff --latest --harness codex)"

# Continue in Codex
codex "$(agent-archive handoff --latest --harness claude)"

# Large session, or an agent without a prompt argument: write a file
agent-archive handoff --latest --output /tmp/handoff.md
# then tell the agent: "Read /tmp/handoff.md and continue the work"
```

macOS `ARG_MAX` is 1 MB, so the default budget is safe as a command
argument.

## Selection

An explicit `SESSION_ID` resolves as `show` does: the local registration
first when `--source` allows it, otherwise `locateMetadataKey`.

`--latest` resolves in this order and prints the choice on stderr
(`handoff: using codex session 7f3c… captured 4 minutes ago`) so a wrong
pick is visible:

1. **Local registrations.** `collector.OpenLocalStoreReadOnly(home)` then
   `LoadRegistrations()`. Keep registrations whose `ProjectRoot` equals the
   project directory or contains it (compared with and without symlinks
   resolved; a root inside the directory does not count, so running from ~
   does not match every project), match `--harness`, and exclude subagent
   registrations (`ParentSessionID != ""`). Order by the transcript file's
   modification time, falling back to `RegisteredAt`, and take the first
   that yields a bundle with a prompt: a session with no transcript yet, an
   unreadable or oversized one, or one that has only just started is passed
   over, never allowed to stop the search. This is what makes a
   same-machine switch immediate.

   Run inside an agent, the newest session is the one running the command.
   `--latest` skips any session whose native ID the environment names:
   `CLAUDE_CODE_SESSION_ID` (observed in Claude Code) and `CODEX_THREAD_ID`
   (read if present, not yet observed). An explicit ID is always honored.
2. **Archive.** If no local registration matches (another machine, or
   `--source archive`), list metadata with `reader.ListMetadataWithOptions`
   and keep sidecars whose `ProjectID == archive.ProjectID(projectRoot)`,
   ordered by `CapturedAt`. Exclude sessions with a `ParentSessionID`.
3. If still nothing matches, exit 1. The message names what was searched
   and, when the archive holds any sessions, lists the five most recent
   non-subagent ones (filtered by `--harness` when given) so one can be
   picked without a separate `list`:

   ```
   agent-archive: handoff: no session for this project on this machine or
   under its project ID in the archive. Recent archived sessions:
     codex   12 minutes ago  another machine  agent-archive handoff 7f3c…
     claude  2 hours ago     this machine     agent-archive handoff 91ab…
     …
   ```

   Rows show only metadata (harness, `CapturedAt` as a relative time, and
   whether `MachineID` equals this machine's), so the fallback keeps the
   rule that listing never prints transcript content. `list` itself gains
   no project column in v1: a name taken from local config would be blank
   on the other machine, which is where it would be needed.

Known limit: `ProjectID` hashes the absolute path, so on a second Mac the
same repository only matches when it is checked out at the same path.
Phase 2 adds a portable repository key (see [Later](#later-phase-2)). Until
then, the cross-machine flow is step 3's fallback list, or `list` →
`handoff ID`.

## Content source

Both sources produce an `archive.SourceBundle`; everything after that is
shared, so the two paths cannot render differently.

- **local** — the registration exists and its `TranscriptPath` is readable.
  Open the file, run the harness's `archive.NewAdapter(name).FilterJSONL`
  (Cursor text transcripts use `FilterText`), and build the bundle in memory
  with `archive.NewSourceBundle`. Merge supplemental evidence already on
  disk from `LoadPublished`/pending requests when present, so hook-reported
  finals and models are not lost. No lock is taken, nothing is written, and
  nothing is uploaded; this read is safe while the collector runs because
  the transcript is append-only and the adapter already tolerates a torn
  last line.
- **archive** — `reader.RefreshAndLoad` on the metadata key, exactly as
  `show --normalized` does, including its `ErrRefreshRequired` message.
- **auto** — local when available, otherwise archive. Any local failure
  (missing, unreadable, oversized, or unsafe transcript) falls back to the
  archive's published copy; if that fails too, both reasons are reported.
  The header records which one was used.

A session ID is accepted only if it is made of the characters archive
session IDs use (`archive.MetadataObjectKey` validates it), because it names
local files and bucket keys.

Session times come from the transcript's own timestamps, then published
metadata (start and capture time), then what this machine knows (the
registration's start, the transcript's modification time). The time the
handoff is built is never used. A Cursor text transcript, which has role
sections rather than records, is rendered from those sections.

`--file PATH --harness NAME` is the local path without a registration: it
filters an arbitrary native transcript. It exists so a same-machine switch
works for sessions the archive declined (started before activation, resumed
old sessions, excluded projects). It never uploads and never registers.

The rendered document is built only from the filtered bundle. The raw
transcript is never read for rendering, only passed through the adapter.

## Handoff model

New file `internal/archive/handoff.go`, pure (no filesystem, network, or
clock), next to `views.go`:

```go
type HandoffOptions struct {
    MaxBytes        int  // 0 = unlimited
    ToolResultLines int  // head+tail lines kept per result before budgeting; default 12
    Preamble        bool
}

type Handoff struct {
    Session    HandoffSession     // ids, harness, models, started/captured, state, turn outcome, source
    Workspace  HandoffWorkspace   // recorded cwd basename and branch
    Plan       *HandoffPlan       // last TodoWrite / update_plan state, if any
    FilesTouched []string         // from Edit/Write/MultiEdit/apply_patch arguments, first-touch order
    Exchanges  []HandoffExchange  // one per human prompt
    LeftOff    string             // last assistant text
    Elisions   []HandoffElision   // what the budget removed, for the footer
    Gaps       []CaptureGap       // bundle capture gaps, codes and counts only
}

type HandoffExchange struct {
    Prompt    NormalizedTurn
    Steps     []HandoffStep       // assistant text and tool calls, in record order
}

type HandoffStep struct {
    Text       string             // assistant text, or ""
    Tool       *HandoffToolCall
}

type HandoffToolCall struct {
    Name, Summary string          // Summary: one line derived from arguments
    IsError       bool
    Result        string          // trimmed result text; "" when none observed
    ResultBytes   int
}

func BuildHandoff(bundle SourceBundle, meta *Metadata, opts HandoffOptions) (Handoff, error)
func RenderHandoffMarkdown(h Handoff) []byte
```

`BuildHandoff` calls `ParseNormalized`, groups turns into exchanges at each
`TurnKindHumanPrompt`, and attaches tool calls by `RecordIndex`. Turns of
every non-prompt user kind added by C1 (`harness_meta`, `command_output`,
`shell_command`, `local_command`) and by H1 below are skipped, except that a
`shell_command` renders as a step (`$ cmd`) because the person ran it.
Content before the first human prompt is attached to a synthetic first
exchange only if it has assistant text.

**Tool call summary.** One line per call, from the retained arguments:

| Tool (any harness spelling) | Summary |
|---|---|
| `Bash`, `exec_command`, `shell`, Codex `CommandExecution` | the command, first line, ≤200 chars |
| `Read`, `read_file` | path, plus range when given |
| `Edit`, `MultiEdit`, `Write`, `apply_patch` | path(s); never the body |
| `Grep`, `Glob`, `search` | pattern and path |
| `Agent`, `Task` | description, else the first 120 chars of the prompt |
| anything else | `name` plus compact JSON of arguments, ≤200 chars |

Edit bodies are excluded on purpose: the receiving agent should read the
file as it is now, not replay diffs that may no longer apply.

**Tool result text.** Add an unexported
`toolResultText(record map[string]any, callID string) string` in
`views.go` beside `toolResultOutput`, using the same shapes plus the Codex
list form fixed in H1. It is exposed only through `BuildHandoff`; `show
--normalized` keeps reporting sizes only. Results are trimmed to the first
and last `ToolResultLines/2` lines with a `… N lines omitted …` marker.
Cursor transcripts carry no tool results; the header says so once instead
of every call showing nothing.

**Plan.** The arguments of the last `TodoWrite` (Claude) or `update_plan`
(Codex) call, rendered as a checklist. This is usually the best statement of
remaining work.

## Markdown layout

```markdown
<!-- agent-archive handoff v1 · claude · session 7f3c… · source: local -->
# Handoff: continuing a Claude Code session

> You are picking up work another coding agent started. The conversation
> below is a filtered record: injected instructions and credentials were
> removed, tool output is trimmed, and edit bodies are omitted. Before
> acting, check the repository's current state (`git status`, the files
> listed below) rather than trusting the record. Ask the person if the next
> step is unclear.

## Session
- Agent: Claude Code 2.1.280 · models: claude-opus-5-5
- Started 2026-09-22 17:02 UTC · last activity 2026-09-22 20:11 UTC · state: idle
- Branch: fix/prompt-accuracy · directory: agent-archive (as recorded)

## Where it left off
<last assistant text>

## Plan
- [x] …
- [ ] …

## Files touched
internal/archive/views.go, internal/archive/types.go, …

## Conversation
### 1 · Person
<prompt, full>

**Agent:** <text>
- `Bash` go test ./internal/archive → ok (12 lines)
- `Edit` internal/archive/views.go
- `Read` docs/agent-archive-privacy.md

### 2 · Person
…

---
Omitted to fit 120 KB: tool output for exchanges 1–9; 41 tool calls in
exchanges 1–4 collapsed. Full record:
~/.agent-archive/handoffs/7f3c….md (read it for anything omitted here).
Capture gaps: sensitive_content_redacted ×1, hidden_instruction_omitted ×2.
```

"Where it left off" and "Plan" come before the conversation so an agent
that reads only the top still has the essentials.

### Workspace section

From the bundle only: the last retained `cwd` (basename only in the
rendered text) and `gitBranch`, labeled "as recorded". The command does not
run `git`. `handoff` runs on the machine where work continues, so a probe
would describe the receiving workspace, not the source one. On the same
machine the receiving agent can run `git status` itself, and the preamble
tells it to. On another machine the useful fact is whether the source left
unpushed work, which only the source can report; phase 2 records it at the
Stop hook (see [Later](#later-phase-2)).

## Budget

Render, measure, and if over `MaxBytes` apply these steps in order,
re-measuring after each, until it fits:

The first three steps never touch the protected tail: the steps of the last
3 exchanges, but no more than the last 20 steps overall. (Protecting three
whole exchanges would leave a session of one prompt and hundreds of tool
calls untrimmable.)

1. Drop tool result text, oldest first.
2. Collapse tool-call lines to a count (`- 14 tool calls: Bash ×9, Read ×5`)
   at the position of the first collapsed call, oldest first.
3. Replace assistant text with its first 300 chars, oldest first. "Where it
   left off" is never shortened.
4. Truncate person prompts longer than 2,000 chars to their first 2,000,
   oldest first. Prompts are never dropped.
5. If still over, emit it anyway and warn on stderr with the final size.

Each step appends a `HandoffElision` so the footer states what was removed.
The budget counts the whole document including preamble and footer.

### Full version when trimmed

When any budget step ran, the CLI also renders the same `Handoff` with no
budget and writes it to
`<AGENT_ARCHIVE_HOME>/handoffs/<archive-session-id>.md` (for `--file`, the
native session ID's SHA-256 prefix stands in for the archive ID). The
trimmed document's footer names that path and tells the agent to read it
for anything omitted. This makes the budget decide what the agent reads up
front, not what it can reach.

- The directory is `0700` and the file `0600`, written atomically with
  `local.WriteBytes`. A later handoff of the same session replaces it.
- Contents are the same filtered rendering, so the privacy guarantees are
  unchanged; it simply is not trimmed.
- The receiving agent is always on the machine where `handoff` ran, so the
  path resolves for it in both same-machine and cross-machine use.
- Files older than 7 days are deleted at the start of every `handoff` run
  and on each collector pass. `uninstall --delete-local-data` removes the
  directory once `handoffs` is in its `localStateEntries` allowlist
  (without that, uninstall reports it as an unknown leftover).
- Nothing is saved when the output was not trimmed, or with
  `--max-bytes 0`. A failure to save warns on stderr, drops the footer
  line, and still emits the trimmed document.
- `--format json` saves the untrimmed JSON with a `.json` extension.

## Prerequisite parser and filter fixes (PR H1)

Found by the probe; none are in C1's or C4's scope. Bumps to filter 6 /
adapter 0.6.0 / parser 0.9.0 (C4 took filter 5 / parser 0.8.0 first).

1. **Codex list-shaped tool output.** `function_call_output.output` and
   `custom_tool_call_output.output` are lists of `{type: input_text, text}`
   blocks in current Codex. `toolResultOutput` handles only strings, so every
   Codex result reports `output_bytes: 0` today. Fall back to `contentText`
   on a list. Fixture: two outputs from a real rollout, hand-sanitized.
2. **Claude harness notifications counted as prompts.** Claude Code writes
   background-task completions as user records with
   `origin: {kind: "task-notification"}` and `promptSource: "system"`; real
   prompts carry `origin.kind: "human"`. On the probed session 12 of 20
   "prompts" were notifications. Allowlist `origin` (only its `kind`
   string) and `promptSource`, and classify a user record whose
   `origin.kind` is a known harness kind (`task-notification`) as a new
   `TurnKindHarnessNotification`, not a prompt. The list is an allowlist:
   an origin kind not on it stays a prompt, so a kind a later version adds
   can never silently drop something the person sent. Filter-5 and older bundles lack the
   field and keep today's behavior.
3. **Codex injected context counted as the first prompt.** Codex prepends a
   user message holding `<recommended_plugins>…</recommended_plugins>`
   (about 50 lines of plugin names). Add the tag to the injected-block list
   in `stripInjectedInstructions` with the same `hidden_instruction_omitted`
   gap. While there, collect every distinct leading `<tag>` across the
   Codex rollouts on the test machine and add any other harness-written one.
4. Not fixable from the record: the desktop app's "The app was quit while
   you were working…" message has `promptSource: "sdk"` and no `origin`,
   exactly like an SDK prompt. Leave it as a prompt and record the reason
   in `docs/capture-capabilities.md`.

## Privacy

- Printing content requires the explicit `handoff` command, as
  `show --normalized` does; `list` stays metadata-only.
- All rendered text comes from filtered records. Local mode runs the same
  adapter as the collector; a test asserts a planted credential in a local
  transcript is `[REDACTED]` in the handoff.
- `--output` creates files `0600` and never inside the project directory
  unless the path given explicitly is there.
- The saved full version lives only in the archive's own data directory,
  with the same permissions and a 7-day lifetime.
- The preamble tells the receiving agent the record is filtered, so it does
  not treat a `[REDACTED]` token as a real value.

## Code layout

| File | Change |
|---|---|
| `internal/archive/handoff.go` | `HandoffOptions`, `Handoff` types, `BuildHandoff`, budget, `RenderHandoffMarkdown` |
| `internal/archive/views.go` | `toolResultText`; H1 fixes |
| `internal/archive/adapters.go` | H1 allowlist and injected-tag changes |
| `internal/collector/snapshot.go` | `ReadLocalBundle` and `FilterTranscriptFile`: the collector's own filtering, read-only |
| `internal/cli/handoff.go` | flag parsing, selection and fallback list, content source, output, full-version save and pruning |
| `internal/cli/collect.go` | prune `handoffs/` files older than 7 days on each collector pass |
| `internal/cli/uninstall.go` | add `handoffs` to `localStateEntries` so `--delete-local-data` removes it |
| `internal/cli/cli.go` | `case "handoff"` dispatch |
| `internal/cli/help.go` | `handoff` help entry and command-guide line |
| `docs/install.md`, `README.md` | command list and recipes |
| `docs/capture-capabilities.md` | per-harness handoff fidelity (Cursor: no tool results) |

## Tests

- **Golden files** in `internal/archive/testdata/handoff/` (new; add an `-update` test flag): one synthetic
  transcript per harness (prompt, text, tool call with result, error
  result, edit, plan) and its expected markdown. Regenerate with
  `go test ./internal/archive -run Handoff -update`.
- **Budget**: a generated 2 MB transcript renders under 120 KB, keeps every
  prompt and the last 3 exchanges' results, and lists elisions in order.
  A budget smaller than the prompts alone still emits and warns.
- **Parity**: the same fixture rendered via local source and via an
  archive round trip through `storage.NewMemoryStore` is byte-identical apart
  from the `source:` header field.
- **Selection**: `--latest` prefers local over archive; subdirectory cwd
  matches its project; subagent registrations are skipped; `--harness`
  filters; no match exits 1 with the search description.
- **Privacy**: planted `sk-`, `AKIA`, and PEM values in a local transcript
  are redacted in output; `--output` mode is `0600`; overwrite refused
  without `--force`.
- **Full version**: a trimmed handoff saves an untrimmed file whose path
  the footer names; an untrimmed one saves nothing; files older than 7 days
  are pruned; a save failure still emits the trimmed document.
- **Fallback list**: with no match, the error lists at most five recent
  sessions, marks this machine versus another, and contains no transcript
  text.
- **H1**: fixtures for each of the three fixes, and parser-version gating
  so filter-5 and older bundles classify as before.
- **Live check** (not CI): with the recipe in the local e2e notes, publish
  copies of one real transcript per harness to MinIO, run
  `handoff ID --source archive` and `handoff --file PATH`, and paste each
  into the other two agents. Record whether each agent correctly states the
  task and the next step. This is the acceptance criterion that matters.

## Sequencing

1. **C1** (`fix/prompt-accuracy`, in progress in another session) merges
   first. It rewrites the user-turn classification in `views.go` that H1
   and H2 extend; starting before it lands means a painful rebase.
2. **H1** — parser/filter fixes above. Implementer B's track.
3. **H2** — `internal/archive/handoff.go` with golden and budget tests.
   Pure; can start in parallel with H1 against C1's branch, rebasing onto
   H1 before review.
4. **H3** — `internal/cli/handoff.go`, selection and fallback list, local
   source, full-version save, help, docs.

Each PR follows the existing review process in
`docs/agent-archive-review-fix-plan.md`.

## Later (phase 2)

- **Portable repository key** so `--latest` works across machines: record
  `repo_key = sha256(normalized origin remote URL)[:16]` in metadata
  (schema bump), computed by the hook from `git config remote.origin.url`
  and never storing the URL itself. `--latest` matches on it when
  `ProjectID` does not.
- **Source workspace state at stop.** The Stop hook records the current
  commit and a count of uncommitted files as supplemental evidence (counts
  only, never names). The handoff then warns "the source left 3 uncommitted
  files at a1b2c3; they are not on this machine" when it matters.
- **Project name and filter for `list`.** With the repository key, record
  the project directory's basename in metadata, add `list --project`, and
  document the new field in `docs/agent-archive-privacy.md`.
- `--include-subagents`: render linked child sessions as nested sections.
- `--since-prompt N`: hand off only the tail of a long session.
- `handoff --copy` to put the output on the clipboard via `pbcopy`.

## Decisions

Settled 2026-09-22.

1. **Budget stays 120 KB** (≈30k tokens), and the untrimmed version is
   always saved when anything is trimmed.
2. **No `git` in v1.** The workspace line shows only the recorded branch
   and directory; recording source-side state at the Stop hook is phase 2.
3. **No project column in `list` for v1.** `--latest` lists recent sessions
   when it finds no match; a portable repository key and a project name in
   metadata are phase 2.
