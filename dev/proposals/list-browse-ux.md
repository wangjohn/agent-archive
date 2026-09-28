# List and browse UX: implementation plan

> **Implementation in review.** This is the original design proposal, not the
> current CLI contract. See [list and show](../../docs/guides/list-and-show.md)
> for released behavior.

Status: prepared 2026-09-28; implementation is under review in [Phase 1 (#99)](https://github.com/wangjohn/agent-archive/pull/99), [Phase 2 (#100)](https://github.com/wangjohn/agent-archive/pull/100), and [Phase 3 (#101)](https://github.com/wangjohn/agent-archive/pull/101). These draft PRs are stacked and have not been released. The “current state” below records the baseline when the proposal was written; decisions and scope may differ in the implementation.

## Purpose

`agent-archive list` is hard to scan: the primary column is a 32-character session ID, timestamps are absolute ISO-8601, and secondary fields (origin, parser, skills) crowd out anything a person can recognize. There is also no interactive way to pick a session and open `show`.

This plan improves browsing in three phases:

1. **Readable list + interactive pick** using metadata already available (no schema change).
2. **Privacy-safe titles in metadata** so the session name can become the primary label.
3. **Polish**: fuzzy title lookup, optional rename, richer TTY layout.

Phase 1 is useful alone. Phase 2 is the lasting fix for “session name first.” Phase 3 depends on Phase 2.

## Constraints (non-negotiable)

These come from the [archive design](../specs/archive.md) and the current `list` / `show` contracts:

| Constraint | Implication |
| --- | --- |
| Metadata contains **no transcript text** today | Phase 1 cannot invent real titles; it can only rearrange identity and time |
| `list` downloads **metadata only** | Any title used by `list` must live in the sidecar (Phase 2), never require a source Get |
| `--json` and redirected stdout are for scripts | Interactive mode only when stdin **and** stdout are TTYs; `--json` never enters a picker |
| Strings printed to a TTY go through `archive.DisplayLine` / `DisplayJSON` | Titles and labels stay escape-safe like every other cell |
| Optional metadata fields do not bump `MetadataSchemaVersion` | Phase 2 can add `title` without an incompatible schema bump; bump `DefaultParserVersion` ([versions](../maintainers/versions.md)) |

## Baseline when proposed

`runListCommand` in `internal/cli/inspect.go` prints:

```text
SESSION  HARNESS  CAPTURED  ORIGIN  PARSER  MODELS  SKILLS USED
```

with the full `session_id`, `formatTimeOrNever` (RFC 3339), and origin/parser always shown. `show` requires a `SESSION_ID`. Bare `agent-archive` prints usage. Relative ages already exist as `relativeAge` (used by `status` and `handoff`). Setup already has a numbered `prompter.menu` for interactive choices.

Metadata has `project_id` (hash) but not `project_root`. Local `config.json` holds included project roots and IDs, so Phase 1 can resolve a basename **on this Mac** when the session’s project is configured here. Remote-only or other-machine sessions stay without a project label until Phase 2 stores one in metadata.

## Target experience

### After Phase 1 (TTY, default `list`)

```text
SESSION   WHEN      HARNESS  PROJECT         MODEL
03e60c25  1h ago    claude   agent-archive   claude-fable-5-1
074f237a  1h ago    claude   agent-archive   claude-opus-5-5  · code-review
…

23 sessions. Enter a number to show details, or q to quit.
› _
```

Selecting a row runs the same path as `show SESSION_ID` (metadata JSON by default). `--json`, pipes, and CI stay non-interactive and keep a stable machine-readable shape.

### After Phase 2

```text
TITLE                              WHEN    HARNESS  PROJECT         ID
Fix flaky OAuth callback tests     1h ago  claude   agent-archive   03e60c25
Investigate list table readability 1h ago  claude   agent-archive   074f237a
```

Full ID remains available (`--verbose`, `show`, `--json`). Titles are filtered and truncated at derive time.

---

## Phase 1 — Readable list and interactive browse

**Goal:** Make the default list scannable and let a person open session details without memorizing a hash. No metadata schema change.

### 1.1 Redesign the default human table

**Files:** `internal/cli/inspect.go`, `internal/cli/help.go`, tests under `internal/cli/inspect*_test.go`, `docs/guides/list-and-show.md`.

Default columns (TTY and plain stdout without `--json`):

| Column | Source | Notes |
| --- | --- | --- |
| `SESSION` | first 8 chars of `session_id` | Stable enough to disambiguate in practice; full ID in `--verbose` and `--json` |
| `WHEN` | `relativeAge(now, CapturedAt)` | Reuse existing helper; keep absolute time in `--verbose` |
| `HARNESS` | `harness.name` | Unchanged |
| `PROJECT` | basename of local config root matching `project_id`, else `-` | Best-effort; never invent a path from the hash alone |
| `MODEL` | first/primary model from existing `modelNames` | Truncate long lists; full set in `--verbose` |
| trailing skill hint | optional `· skill` when skills used | Dim when color is on; omit the separate SKILLS column |

**Move out of the default row** (keep via `--verbose` and always in `--json`):

- full `session_id`
- absolute `captured_at`
- `origin` (imported / hook)
- `parser` status
- full skills list as its own column

Add `--verbose` / `-v` to `list` for the wide ops-oriented table (or a multi-line detail block). Prefer one extra flag over changing `--json`.

**Ambiguity:** If two listed sessions share an 8-char prefix (extremely rare), print enough extra characters in that run to make the short IDs unique, or fall back to the full ID for those rows only.

**Count line:** Keep `N session(s).` after the table.

### 1.2 Keep `--json` and filters stable

Do **not** change the `list --json` document shape (`schema_version`, `sessions`, optional `unavailable`). Filters (`--harness`, `--model`, `--skill`, `--since`, `--complete`, `--imported`, `--hook-captured`, `--no-cache`) behave as today. Only the human table changes.

Update golden/assertion tests that hard-code the header row or expect a full ID as the first field of each line (`listedSessionIDs` in `inspect_test.go` must learn short IDs or read from `--verbose` / `--json`).

### 1.3 Interactive picker on TTY

**When interactive** (all of the following):

- stdin is a terminal
- stdout is a terminal
- `--json` is not set
- command is `list` with no “print and exit only” override, **or** `show` with no `SESSION_ID`

**When not interactive:** current non-interactive behavior (print table / require ID / print JSON).

**First implementation: numbered menu**, not a full TUI.

Rationale: the CLI already uses `prompter.menu` in setup; no new dependency; easy to drive in tests with a `strings.Reader`. A bubbletea/fzf-style browser can wait for Phase 3 if numbered selection feels too slow at large N.

Behavior for interactive `list`:

1. Fetch and filter sessions as today.
2. Print the redesigned table with a leading index column (`1)`, `2)`, …) or reuse the short ID as the selectable token.
3. Prompt: `Enter number (or short id) to show, or q to quit`.
4. On selection, print that session’s metadata the same way `show` does (without `--normalized` unless a later flag asks for it). Optionally loop (“show another?”) so one `list` can inspect several sessions; Ctrl-C / `q` exits 0.
5. Cap the interactive page (for example first 50, with “showing 50 of N; refine with --since / --harness”) so a huge import does not dump an unusable menu. Exact cap is an implementation choice; document it in help.

Behavior for interactive `show` with no argument:

1. Same listing + picker as interactive `list` (shared helper).
2. Selecting one session prints `show` output and exits (no loop), matching “I wanted to look at one session.”

Add `--no-interactive` (or rely solely on pipe detection) so scripts and demos can force the plain table. Prefer detecting TTY only first; add an explicit flag only if needed for tests or user requests.

**Do not** change bare `agent-archive` (no args) in Phase 1: it keeps printing usage / “Not set up yet.” Turning the default command into browse is Phase 3 optional polish and is easy to get wrong for muscle memory.

### 1.4 Shared list rendering helper

Extract formatting so `list`, interactive `show`, and (later) title-first layout share one path:

- `formatSessionRows(sessions, opts) → []row` with short ID, when, harness, project label, model, skill hint, verbose fields
- `printSessionTable(w, rows, style)`
- `resolveProjectLabel(cfg, projectID) string` reading included projects from loaded config (empty config → `-`)

`handoff`’s “recent sessions” hint can optionally reuse short ID + relative time later; out of scope unless it is cheap.

### 1.5 Phase 1 acceptance criteria

- [ ] Default `list` is readable at a glance: short ID, relative time, no origin/parser columns
- [ ] `list --json` byte-compatible in schema (same fields; still full metadata objects)
- [ ] `list --verbose` still exposes full ID, absolute time, origin, parser, skills
- [ ] TTY `list` can select a session and print `show`-equivalent metadata
- [ ] TTY `show` with no ID opens the same picker; non-TTY `show` still errors with “SESSION_ID is required”
- [ ] Piped `list` never blocks on a prompt
- [ ] Docs and `--help` describe the new table and interactive behavior
- [ ] Existing inspect/list tests updated; new tests for short IDs, relative time, TTY vs non-TTY picker

### 1.6 Phase 1 risks

| Risk | Mitigation |
| --- | --- |
| Short ID collisions | Lengthen only colliding rows; tests with crafted IDs |
| Project label missing for imports / other machines | Show `-`; Phase 2 stores a stable project name in metadata |
| Interactive loop surprises automation | Gate on TTY; never on `--json` |
| Test churn in `listedSessionIDs` | Prefer asserting via `--json` for identity; treat human table as display |

---

## Phase 2 — Privacy-safe titles in metadata

**Goal:** Give every archived session a human label derived without putting raw transcript bodies into `list`, and make that label the primary list column.

### 2.1 Metadata field

Add an optional field on the sidecar (names bikeshedable at implement time; recommended):

```json
"title": "Fix flaky OAuth callback tests"
```

Rules:

- Derived in `archive.BuildMetadata` / parser path from the **first** `TurnKindHumanPrompt` text in the normalized view (already filtered).
- Collapse whitespace to one line; truncate to a fixed rune budget (recommend **72** display runes); append ellipsis when truncated.
- Empty / whitespace-only / missing prompt → omit `title` or set a fallback such as `Untitled session` (prefer omit + CLI fallback `"untitled"` so JSON stays honest).
- Must pass through the same display-safety rules as other printed strings.
- Optional companion later (same phase or Phase 3): `project_name` (basename of `ProjectRoot` from the registration/bundle at publish time) so remote `list` shows a project without local config.

Update `schemas/metadata.schema.json` with the optional property. **Do not** bump `MetadataSchemaVersion` (optional field). **Do** bump `DefaultParserVersion` because derived metadata changes ([versions](../maintainers/versions.md)).

### 2.2 Backfill and re-derive

Existing sessions get titles when:

- the collector re-derives metadata after a parser bump (normal path), or
- the user runs a sync/collect pass that republishes metadata for unchanged sources

Document that until re-derive, `list` falls back to short ID as the primary label (or shows `-` in the title column). No separate migration job required if parser bump already triggers metadata refresh for retained sources.

### 2.3 List column flip

Once titles exist for most sessions:

| Column | Role |
| --- | --- |
| `TITLE` | Primary; fallback to short ID when missing |
| `WHEN` | Relative |
| `HARNESS` | |
| `PROJECT` | Prefer metadata `project_name` when present, else local resolve |
| `ID` | Short ID secondary |

Interactive picker labels become `title` (dim short ID). `--json` includes `title` on each metadata object automatically.

### 2.4 Privacy

Titles are filtered prompt text. They can still contain sensitive phrases the filter did not catch. Document in [privacy](../../docs/security/privacy.md) / list guide: titles are a short preview of the first user prompt, stored in the bucket like other metadata. Do not open a path that pulls unfiltered transcript into the sidecar.

### 2.5 Phase 2 acceptance criteria

- [ ] New sessions get a `title` in metadata when a human prompt exists
- [ ] `list` (human) shows title first; missing title falls back cleanly
- [ ] `list` still does not download source bundles
- [ ] Parser version bumped; schema updated; goldens regenerated as needed
- [ ] Docs describe title derivation and fallback

### 2.6 Phase 2 risks

| Risk | Mitigation |
| --- | --- |
| Vague first prompts (“fix this”) | Accept for v1; optional rename in Phase 3 |
| Title leaks more than users expect | Document; same filter as archived content; keep short |
| Old sidecars without title | Fallback to short ID; parser bump refreshes |

---

## Phase 3 — Polish (after Phase 2)

Ordered by value; each can ship separately.

1. **Fuzzy / substring `show` by title** when the argument is not a full session ID (disambiguate with a picker on multiple matches).
2. **`agent-archive feedback` / rename**: optional user-supplied title override stored in metadata or a small sidecar note (design carefully so overrides survive republish).
3. **Full TUI browser** (arrow keys, `/` filter) if numbered menus hurt at hundreds of sessions; still TTY-only.
4. **Optional:** bare `agent-archive` on a TTY runs interactive list when already set up; non-TTY keeps usage.
5. **Card / multi-line TTY layout** when the terminal is wide enough and titles exist.
6. **Group by project** headings in the human list.

---

## Suggested implementation order

| Step | Work | Depends on |
| --- | --- | --- |
| A | Extract row formatting; short ID + `relativeAge`; drop origin/parser from default; `--verbose` | — |
| B | Local project basename resolution | A |
| C | Update tests/docs/help for the new table | A, B |
| D | Shared interactive picker; wire TTY `list` and bare TTY `show` | A, C |
| E | Parser: derive `title`; schema; parser bump | — (can parallelize after A starts) |
| F | Flip list primary column to title; picker labels | D, E |
| G | Phase 3 items as follow-ups | F |

**Ship Phase 1 (A–D) as one PR or a tight PR stack.** Ship Phase 2 (E–F) as a second change so review can focus on privacy and schema. Keep Phase 3 as issues/follow-ups after both land.

## Out of scope

- Changing retention, capture hooks, or bucket layout
- Downloading transcripts during `list`
- MCP or GUI clients (CLI only)
- Making `handoff --latest` interactive (related, but separate)
- Linux/cloud capture ([cloud-capture](cloud-capture.md))

## Documentation touch list

When implementing:

- `docs/guides/list-and-show.md` — primary user guide
- `internal/cli/help.go` — `list` / `show` usage
- `docs/reference/cli.md` if flags are listed there
- `CHANGELOG.md` under Unreleased
- Phase 2 only: `schemas/metadata.schema.json`, `dev/maintainers/versions.md`, privacy docs if title preview needs a user-facing note

## Open questions for review

1. **Short ID length:** 8 chars (git-like) vs 12? Recommendation: 8 with collision stretch.
2. **Interactive after `list`:** one-shot select-and-exit vs loop until `q`? Recommendation: loop for `list`, one-shot for bare `show`.
3. **Title fallback string:** omit field vs `"untitled"`? Recommendation: omit in JSON; print `untitled` or short ID in the table.
4. **Store `project_name` in Phase 2** alongside `title`? Recommendation: yes if cheap at publish time; otherwise leave for a small follow-up.
5. **Page size for interactive list:** 50? 100? Recommendation: 50 with a clear “refine filters” hint.

## Success metrics (qualitative)

- A new user can find “the session I just had in this repo” without copying a hash from another tool.
- `list \| grep` and `list --json` keep working for scripts.
- No path from `list` to unfiltered transcript content.
