# Finding a session — engineering plan

Status: planned 2026-09-30, [decisions](#decisions) confirmed the same day;
not started. Where this plan and the code differ once packages merge, the
code is the reference and differences go under Deviations.

Goal: the session a person means is on the first screen of the handoff
picker or `list` without typing, and one or two words find it when it is
not. The same words find it when an agent (Codex, Claude Code, Cursor) runs
`agent-archive handoff "<words>"` or `agent-archive list "<words>"` for the
person.

Related: the handoff performance work (another session, 2026-09-30) is
making the archive listing faster in `internal/reader` and `internal/storage`,
and will later spec an index read. This plan owns the picker, `list`'s rows
and browser, the parser, and the filter; that work owns listing speed. The
index spec is to provide a subagent count per parent (from `LinkedSessions`),
one parent's children on demand, a repository-scoped window of recent
top-level sessions that falls back to `ProjectID`, and an unscoped one. Search
keeps the full scan or the local cache, so the matcher's fields must stay in
the sidecar.

## Decisions

Confirmed 2026-09-30:

1. **`list` hides subagents too.** Its table and browser show top-level
   sessions only, like the picker; bare `show` does too, since it shares the
   browser. A parent carries a `· N subagents` hint,
   and a subagent is reached by searching. `list --json` keeps every row
   ([§1](#1-top-level-sessions-only)).
2. **Default to this repository, with a fallback to all projects, and make
   the scope easy to change.** The browser (`show`, `list`, `handoff`) and title search all start
   from the working directory's repository. When nothing there matches, they
   fall back to all projects and say so. `a` toggles the scope on screen, and
   `--all-projects` and `--project` change it on the command line
   ([§3](#3-scope-this-repository-first)).
3. **`list` takes a search query, with the same UI as the picker.** `list
   "<words>"` and the picker share the matcher, the row formatter, the
   browser, the `/` filter, and the scope heading. The only difference is
   what Enter does: `list` shows the session, the picker hands it off
   ([§4](#4-one-matcher), [§5](#5-one-browser-filter-as-you-type)).
4. **Subagent descriptions come from `.meta.json`**
   ([§6](#6-subagent-descriptions)).
5. **The live marker `●` reuses `activeSourceWindow` (2 minutes)**, so the
   dot and handoff's shared-checkout question agree ([§7](#7-rows)).
6. **Every session chooser is one component.** Bare `show`, `list`, and
   `handoff`, and every place they ask the person to pick one session, use
   the same browser. That includes `show --json` with no ID, and an
   ambiguous `show "<words>"` or `handoff "<words>"`. Differences are
   parameters, not separate code ([§5](#5-one-browser-filter-as-you-type)).

## Problem

Measured on the owner's Mac on 2026-09-30 (662 registrations: 653 Claude,
8 Cursor, 1 Codex):

1. **Subagents crowd out sessions.** 558 of the 662 registrations are
   subagents; 104 are top-level. The picker means to offer top-level
   sessions only, and `rows` drops subagent *registrations*
   (`topLevelRegistration`), but the archived list from
   `loadSessionsForBrowse` is never passed through `topLevelSessions`
   (`internal/cli/handoff_select.go`, the `for _, m := range archived`
   loop). So every uploaded subagent is offered. In one 45-row screen, 34
   rows were subagents of four orchestrator sessions (45, 33, 24 and 17
   children). The footer said "Showing 50 of 659"; about 100 could be
   handed off. `TestHandoffPickerAndToSkipSubagents` covers only a local
   subagent registration. Title search already filters archived subagents
   (`handoff_title.go`, `archiveMatches`). `list` shows every subagent as a
   row of its own, with nothing that names its parent.
2. **Titles show the least distinctive text.** A title is the first
   filtered human prompt, cut to 72 runes. Prompts an orchestrator writes
   start with the same preamble ("You are the REVIEWER AND FIXER for
   https://github.com/wangj…", "In the agent-archive repo (Go CLI, …"), so
   10 rows can read alike. The harnesses already record a better name, and
   the archive drops it:
   - Claude Code writes `{"type":"custom-title","customTitle":"Fix flaky
     retention hook-request test"}` (the name in the app's sidebar, set
     automatically or by `/rename`; the last record wins). 92 of the 95
     top-level Claude sessions have one.
   - Claude Code writes `{"type":"pr-link","prNumber":"213","prRepository":
     "wangjohn/agent-archive","prUrl":…}` for a PR the session opened or
     linked. 64 of 95 have one.
   - Each Claude subagent transcript `subagents/agent-<id>.jsonl` has a
     sibling `agent-<id>.meta.json` whose `description` is the name the
     parent gave the task ("Review and fix PR #208 (5b-1b)"). 543 of 561
     have one.
   - Cursor's composer data has a chat `name`. Codex keeps a `thread_name`
     per thread in `~/.codex/session_index.jsonl`.

   The privacy filter drops `custom-title` and `pr-link` as
   `unknown_record_type` (`ClaudeAdapter.FilterJSONL`,
   `internal/archive/adapters.go`), and Cursor's `name` as an omitted chat
   key (`cursorComposerConsumed`). Nothing reads `.meta.json` or
   `session_index.jsonl`.
3. **Columns that never vary.** Run inside a repo, PROJECT and HARNESS are
   the same on every row. Grouping by project (`GroupByProject`) does
   nothing with one project. It also splits one repository's checkouts,
   because `ProjectID` hashes the checkout path.
4. **No way to type what you remember, and two choosers.** `list` and bare
   `show` run the alt-screen key browser (`runSessionBrowser`). The handoff
   picker, `show --json` with no ID, and an ambiguous `show "<words>"` or
   `handoff "<words>"` run the older line-mode chooser (`pickBrowseRow` and
   `pickBrowseSession`). The two look and behave differently: one is a
   full-screen table with keys, the other a printed table with a prompt.
   Neither filters by text; both take only a number or an ID. `list` cannot
   search at all; its help says to use `handoff` for that. `show "<words>"`
   and `handoff "<words>"` match through `matchSessionsByQuery`, but print
   ambiguous matches in different formats.
5. **Agents search one field.** `handoff "<words>"` matches the whole query
   as a substring of the title, or an ID prefix. "hand off my Linux support
   session" works only if the first prompt happens to contain "linux
   support". On several matches it prints a table and exits 1, and there is
   no structured way to see candidates.

## Goals

1. The browser (bare `show`, `list`, and `handoff`) shows only top-level
   sessions. Subagents are found
   by searching, and are shown under their parent.
2. Every row is labeled with the name the person saw in their agent, when
   the harness has one. The first prompt is the fallback.
3. Run inside a repository, the picker, `list`, and title search look at
   that repository's sessions first, across all its checkouts and worktrees.
   One key or one flag widens or moves the scope.
4. Choosing a session looks and works the same in `show`, `list`, and
   `handoff`: one browser, one set of keys. A person can type a few words, a
   PR number, or a branch name to narrow it as they type.
5. One matcher serves the browser's filter, `handoff "<words>"`, `show
   "<words>"`, and `list "<words>"`, so a person and an agent get the same
   answer from the same words.
6. Nothing added leaves the privacy filter's guarantees: every new field
   comes from filtered records.

## Non-goals

- Searching transcript content. It is slow over the archive and noisy (a
  word appears in hundreds of tool results). Fields are metadata only, as
  today.
- Rewriting prompts heuristically (stripping "You are the…" preambles). With
  native names on 97% of top-level Claude sessions, the fallback matters
  rarely, and a heuristic would mislabel the rest.
- An LLM-written title. Titles stay deterministic.
- Flags for what to match. Words carry the query (`handoff "#212"`,
  `list "linux support"`). The only new flag is `--all-projects`, which is
  about where to look.

## Design

### 1. Top-level sessions only

`handoffPicker.rows` drops archived metadata with a `ParentSessionID`, as it
drops subagent registrations. `total` and the footer count only what can be
offered.

`list`'s table and the browser (so bare `show` too) drop subagents the same
way, before `--limit` applies. `--limit 50` therefore means 50 top-level sessions. The footer adds
how many were hidden: `42 sessions (318 subagent sessions hidden; search to
find one)`.

`list --json` keeps every row, subagents included. Each row carries
`parent_session_id`, scripts may count subagents, and changing that would
change the script-facing document's row set. A query ([§4](#4-one-matcher))
narrows `--json` rows like table rows.

Subagents come back through search ([§5](#5-one-browser-filter-as-you-type)),
never as unfiltered rows.

### 2. Names the person recognizes

**Filter (version 13, adapter 0.13.0).**

- Claude: admit `custom-title` with the key `customTitle`, and `pr-link`
  with `prNumber`, `prRepository`, and `prUrl`. `customTitle` is text the
  model wrote from the person's prompt, or the person typed. It passes the
  same `sanitizeValue` redaction as prompt text. `prRepository` and
  `prNumber` must match `git_activity`'s patterns. `prUrl` is kept only
  when it is the GitHub URL rebuilt from those two, else dropped. The
  records' `sessionId` and `timestamp` stay as for other records.
  `agent-name` (a copy of the custom title) and `last-prompt` (derivable
  from the turns) stay dropped.
- Cursor: consume the chat-level `name` (redacted as prompt text) into a
  record the parser reads. It is no longer reported as an omitted key.
- Changelog section, `dev/specs/privacy-filter.md`,
  `docs/security/privacy.md` (a session's name is now archived), and
  regenerated goldens, per [versions](../maintainers/versions.md).

A filter bump makes the collector re-read and republish every session whose
transcript still exists. That is the only way to recover names the old
filter dropped. A session whose transcript is gone keeps its first-prompt
title.

**Parser (0.17.0).** Three optional metadata fields. Optional fields do not
bump the metadata schema version; `schemas/metadata.schema.json`
(`additionalProperties: false`) gains them in the same PR.

| Field | Type | Derived from |
|---|---|---|
| `name` | string, collapsed like `title` (72 runes) | Claude: the last `custom-title`. Cursor: the chat `name`. A Claude subagent: its description ([§6](#6-subagent-descriptions)). Omitted when there is none. |
| `branch` | string, `branchPattern` | The last record's `gitBranch` (as `recordedWorkspace` reads it for handoff). Omitted when empty or `HEAD`. |
| `pull_requests` | array of `{repository, number, url}`, at most 20, first-linked order, deduplicated | Claude `pr-link` records. |

`title` keeps its meaning (the first filtered prompt), so nothing that reads
it changes. Display and search read the new fields through two helpers:

```go
// DisplayTitle is what a row shows: the harness's name, else the title.
func DisplayTitle(m Metadata) string
// LatestPR is the last linked PR, else the last pr_created in git_activity.
func LatestPR(m Metadata) (PullRequestLink, bool)
```

The picker's local rows (`localMetadata`) build their fields with the same
code as `BuildMetadata`. `firstPrompt` gives way to one exported function
both paths call:

```go
// SessionLabels derives name, title, branch and PRs from a filtered bundle.
func SessionLabels(bundle SourceBundle) (Labels, bool)
```

This also removes the one difference between the two today: local titles are
cut by display width, and published ones by runes.

### 3. Scope: this repository first

The browser (`show`, `list`, `handoff`) and title search find a *scope*
from the working
directory:

1. The repository key of the directory (`gitremote.Resolver.Key`, the same
   hash hooks record). A session is in scope when its `RepoKey` equals it.
   This spans checkouts, worktrees, and Macs.
2. With no key on either side (no origin remote, or an older session), a
   local session is in scope when `sameProject(reg.ProjectRoot, dir)`, and an
   archived one when its `ProjectID` is in `projectIDs(dir)`, as `--latest`
   already decides.
3. Outside any project, there is no scope: every session, grouped by project
   as today.

**Changing the scope.** Every way below is named on screen, so none has to
be remembered:

- **`a` on screen.** In the browser, whatever command opened it, `a` (typed alone,
  like `n`, `p` and `q`) toggles between the scope and all projects. The
  heading always names what is shown and the other choice: `agent-archive ·
  42 sessions · a all projects`, or `All projects · 104 sessions · a
  agent-archive`.
- **`--all-projects`** on `handoff` and `list`: no scope.
- **`--project DIR|NAME`** on `handoff` and `list`: the scope of that
  directory instead of the working one. A value that is not a directory is
  matched as a project name, case-insensitively and exactly, against
  `project_name` and configured project labels. `handoff --project` exists
  today for `--latest` only; it now applies to every selection. Given with
  `--all-projects`, it is a usage error.
- **Project names are a search field** ([§4](#4-one-matcher)), so `handoff
  "personal_website blog"` reaches another project without a flag.

**Fallback.** When the scope holds nothing to show, the browser opens on
all projects, and the heading says `Nothing in agent-archive ·
showing all projects`.

Title search, in `handoff "<words>"` and `list "<words>"`, looks in scope
first, then everywhere, as `handoff` already looks on this Mac before the
archive. The tiers, each tried only when the one before has no match:

1. an exact session ID (unchanged)
2. top-level sessions in scope
3. top-level sessions anywhere
4. subagents in scope, then anywhere

When tier 2 answers, a note says how many more match outside the scope,
without listing them: `1 match in agent-archive (3 more in other projects:
--all-projects or a project name finds them)`. `handoff` prints the note on
stderr, and `list` prints it in its footer. `list --json` keeps the same
tiers, and gains an optional `scope` object (`{"label": "agent-archive",
"all_projects": false, "fell_back": false, "outside_matches": 3}`). The
field is additive, so `listSchemaVersion` stays 4.

**Compatibility.** Run in a project, `list` and `list --json` now return
that project's sessions where they returned all of them. Scripts that want
everything pass `--all-projects`. The CLI reference and the changelog say
so.

### 4. One matcher

```go
// sessionQuery is parsed once from what the person or agent typed.
type sessionQuery struct{ words []string; prs []int }
func parseSessionQuery(s string) sessionQuery
// matches reports whether every word appears in some field of the row.
func (q sessionQuery) matches(r sessionFields) bool
```

- The fields are name, title, branch, project name, harness, and short or
  full ID prefix. Every word must appear in at least one field,
  case-insensitively. Words may match different fields, so "linux 212"
  matches the Linux session's name and its PR.
- A word `#N`, or a bare number of 1–6 digits, also matches a PR number
  exactly. A bare number is still a row index when typed at a browser's
  number prompt (see §5).
- There is no score. Order within a tier is newest activity first, as today.
  An exact ID still wins outright.

Callers:

- the browser's `/` filter and the line-mode filter (§5), in both the picker
  and `list`
- `handoff "<words>"`: replaces `matchSessionsByQuery` inside
  `handoffQueryResolver`. The 50-session local scan bound stays, and a
  local session's fields come from `SessionLabels`.
- `list "<words>"`: new. `list` takes one optional positional query. It
  applies the §3 tiers, then `--harness`, `--since`, and the other filters,
  then `--limit`. Interactive, it opens the browser with the query already in
  the `/` filter, so the person can edit it. Piped, it prints the table.
  With `--json`, it prints the document.
- `show "<words>"`: `resolveShowQuery` replaces `matchSessionsByQuery` with
  the matcher and the §3 tiers. Its direct reads are unchanged: an exact
  full ID is still read without listing. Several matches open the browser
  on a terminal. Without one, they print the same candidate table as
  `handoff`, with `Next: agent-archive show <ID>`.

When `handoff "<words>"` matches several sessions without a terminal, the
table gains the columns an agent needs to tell rows apart, and ends with the
exact next command:

```text
agent-archive: handoff: "flaky" matches 3 sessions in agent-archive; pass one ID:
  d7a77938  claude  just now  #213  Fix flaky retention hook-request test
  36a7d5ee  claude  36m ago   #209  Fix flaky hook-lock timeout test bound
  76941c69  claude  12h ago   #183  Fix flaky TestBrowserKeysRestoreTheTerminal
Next: agent-archive handoff d7a77938 --harness claude
      (or: agent-archive list "flaky" --json)
```

A subagent row is labeled `subagent of <parent short ID>`.

### 5. One browser, filter as you type

Every place a person chooses a session runs one component. It is the
alt-screen key browser that `list` and bare `show` use today
(`runSessionBrowser`, `browserKeys`), with one line-mode fallback for when no
key terminal can be opened. It replaces `pickBrowseSession` and
`pickBrowseRow`, whose separate line-mode loop goes away except as that
fallback.

The callers differ only in three parameters: where rows come from, what
Enter does, and the heading's verb.

| Caller | Rows | Enter | Heading |
|---|---|---|---|
| `list` | archive | show the details, then back to the list (browse) | none |
| `show` (no ID) | archive | the same as `list` (browse) | none |
| `show --json` (no ID) | archive | return the session; print its JSON (pick one) | `Show ·` |
| `show "<words>"`, several matches | the matches | show the details (browse) | `"words" matches N ·` |
| `handoff` (no selector) | local and archive, merged as today | return the session to hand off (pick one) | `Hand off ·` |
| `handoff "<words>"`, several matches | the matches | the same as `handoff` (pick one) | `Hand off · "words" matches N ·` |

```go
// browserMode is what Enter does with the chosen row.
type browserMode int // browseSessions: show details and return; pickSession: return the row
type browserSpec struct { Mode browserMode; Verb string; Rows []listRow; Query string }
```

Everything else is the same code for every caller:

- the rows, their formatter (§7), and the subagent folding
- the scope heading and `a`
- `/`, the highlight, and the line-mode filter
- the matcher (§4)
- paging, scrolling, and fitting to the window (#149)

Rows keep their own sources: `list` and `show` read the archive, and
`handoff` also offers this Mac's sessions that are not uploaded yet. Making
`list` and `show` offer local sessions is out of scope. `list`'s ID column
is a column rule (§7), not a browser difference.

The handoff picker, `show --json`, and the two ambiguous-query pickers move
from line mode to the key browser. Their dependency interfaces gain
`openKeyTerminal` and `interrupts`, which `sessionBrowserDependencies`
already has. Every prompt after the handoff picker (destination, active
source) keeps reading through the shared answers reader, as
`prepareLaunchDir` requires. The browser must hand back the terminal with no
typed-ahead input lost.

**Key mode.** `/` opens a filter line at the bottom, as in `less`. Rows
narrow as each character is typed. While filtering:

- The first row is highlighted (▸), and ↑/↓ move the highlight.
- Enter acts on the highlighted row, and Esc clears the filter.
- Row numbers keep their unfiltered values, so a number seen before
  filtering still works.

Outside the filter, keys behave as today: numbers, `n`, `p`, `q`, arrows,
plus `a` for scope.

**Line mode.** An answer that is not a number, an ID, or a command word (`n`,
`p`, `q`, `a`) is a filter. The table redraws with the matching rows and
says `3 sessions match "flaky" · a number, more words, or Enter for all`. An
empty answer clears the filter, and quits when there is none, as today.

**Subagents while filtering.** A matching subagent appears indented under its
parent (`↳ Review and fix PR #208 (5b-1b)`), and the parent is shown even
when it does not match. It can be picked like any row. Unfiltered, a parent
with subagents carries a dim hint where the skill hint goes: `· 45
subagents`. The count comes from the subagent rows already loaded; once the
listing index exists, it comes from the parent's `LinkedSessions`.

### 6. Subagent descriptions

A Claude subagent's `description` is not in its transcript. It sits in the
sibling `agent-<id>.meta.json`. The collector reads that file when it filters
a subagent transcript (it is beside `TranscriptPath`), and prepends one
synthetic record to the filtered records:
`{"type":"subagent-meta","description":…}`.

- The description is redacted as prompt text and length-bounded.
- Nothing else from the file is kept. `worktreePath` names a local path, and
  `agentType` is already local-only state.
- The parser takes `name` from it.
- This is part of the filter (version 14) because it changes filtered
  output.

The alternative was the `description` input of the parent's Agent tool call
in the parent's filtered transcript. It was rejected because joining that
call to the child needs the `toolUseId`, which only the local `.meta.json`
names.

### 7. Rows

For every browser caller (§5):

- **Title:** `DisplayTitle`.
- **PR column:** `#213`, from `LatestPR`. Shown only when some row on screen
  has one.
- **HARNESS and PROJECT:** hidden when every row shown has the same value.
  The heading names that value instead (`agent-archive`, `claude`).
- **Live marker:** a leading `●` on a session active within
  `activeSourceWindow` (2 minutes), as its local activity reports it. This
  is the same test that makes handoff ask about a shared checkout. A session
  waiting on the person for longer shows no dot.
- **ID:** kept in `list`, where people copy it. In the picker it is dim;
  numbers select, and a typed ID is still accepted.

Target, run in agent-archive:

```text
Hand off · agent-archive · 42 sessions                   / filter · a all projects

 #     TITLE                                          PR     WHEN
 1  ●  Fix flaky retention hook-request test          #213   just now
 2  ●  Stop non-hook fsyncs under the request lock           just now
 3  ●  Implement Linux support for agent-archive      #212   1m        · 45 subagents
 4     Investigate a lint for durable writes…                2m
 5     Agent-archive open source readiness            #167   4m        · 17 subagents
 6     Fix termlaunch tests under umask 002           #202   11m
 7     Insights command with agent-archive            #204   12m       · 33 subagents
 8     Fix gitremote os/exec architecture check       #214   24m
```

`/208`, typed in that screen:

```text
Hand off · agent-archive · "208" matches 1                        Esc clear

 #     TITLE                                          PR     WHEN
 3  ●  Implement Linux support for agent-archive      #212   1m
    ▸  ↳ Review and fix PR #208 (5b-1b)               #208   3h
```

## Shared names

Change these here first.

| Owner | Name |
|---|---|
| PR 3 `internal/archive` | `Metadata.Name string` (`json:"name,omitempty"`), `Metadata.Branch string` (`json:"branch,omitempty"`), `Metadata.PullRequests []PullRequestLink` (`json:"pull_requests,omitempty"`); `type PullRequestLink struct { Repository string; Number int; URL string }`; `type Labels struct { Name, Title, Branch string; PullRequests []PullRequestLink }`; `SessionLabels(bundle SourceBundle) (Labels, bool)`; `DisplayTitle(m Metadata) string`; `LatestPR(m Metadata) (PullRequestLink, bool)`; `MaxPullRequests = 20` |
| PR 4 `internal/cli` | `type sessionScope struct { Label string; RepoKey string; Dir string; ProjectIDs []string; All bool }`; `scopeFor(env scopeDependencies, project string, allProjects bool) (sessionScope, error)`; `(sessionScope).contains(m archive.Metadata, reg *archive.SessionRegistration) bool`; `listDocument.Scope *listScope` (`json:"scope,omitempty"`) |
| PR 5 `internal/cli` | `sessionQuery`, `parseSessionQuery`, `(sessionQuery).matches`, `type sessionFields struct { Name, Title, Branch, Project, Harness, SessionID string; PRs []int }`, `fieldsOf(m archive.Metadata, project string) sessionFields`; `listRow.Children int` |
| PR 6 `internal/cli` | `listRow.Depth int`, `listRow.Live bool`; `sessionPicker.filter string`; `type browserMode`, `type browserSpec` (§5); `runBrowser(env sessionBrowserDependencies, spec browserSpec, …) (listRow, bool, int)`, which replaces `pickBrowseSession` and `pickBrowseRow` and backs `runSessionBrowser` |

## PR breakdown

| PR | What | Base | Parallel with |
|---|---|---|---|
| 0 | this plan; pointer from [handoff.md](handoff.md#selection) | main | — |
| 1 | picker drops archived subagents | main | 2, 4 |
| 2 | filter 13: Claude `custom-title`, `pr-link`; Cursor `name` | main | 1, 4 |
| 3 | parser 0.17.0: `name`, `branch`, `pull_requests`; `SessionLabels`; rows show `DisplayTitle` | 2 | 4 |
| 4 | scope for picker, `list`, and title search; `a`, `--all-projects`, `--project`; rows (PR column, hidden constant columns, `●`) | 1 | 2, 3 |
| 5 | one matcher; search tiers; `list "<words>"`; `list` hides subagents with the `· N subagents` hint; candidate table; skill text | 3 + 4 | — |
| 6 | one browser for `show`, `list`, `handoff`, and every ambiguous-query chooser; `/` filter; line-mode filter; subagents under parents | 5 | 7 |
| 7 | filter 14: subagent descriptions from `.meta.json` | 3 | 6 |
| 8 | docs (handoff and list guides, CLI reference, agent-skills guide) and live check | all | — |

`list` hides subagents in PR 5, not PR 1, so that from the moment they leave
the table, `list "<words>"` can find them.

### PR 1 — top-level only in the picker (~20 lines + tests)

Drop archived rows with `ParentSessionID != ""` in `handoffPicker.rows`
before the merge. Tests in `handoff_select_test.go`:

- an archived subagent is not offered
- the footer's total excludes it
- a local subagent registration whose parent is archived is still skipped

### PR 2 — filter 13 (~250 lines + goldens)

`FilterJSONL`'s allowlist gains `custom-title` and `pr-link`, and
`allowedKeys` gains their keys. `pr-link` values are validated against
`git_activity`'s repository and number patterns, and the URL is rebuilt. The
Cursor composer filter consumes `name`. Tests:

- a filter test per record type: kept, redacted, and a malformed `pr-link`
  dropped with a gap
- goldens regenerated
- the changelog's version-13 section lists exactly what is newly kept

### PR 3 — parser 0.17.0 (~300 lines + tests)

`SessionLabels`, the three fields, the schema, `DisplayTitle` in
`formatSessionRows` and `show`'s summary (Name, Branch, and PR lines).
`localMetadata` uses `SessionLabels`, and `firstPrompt` stays only for
`hasPrompt`. Tests:

- the last `custom-title` wins
- PRs are deduplicated and capped
- a `HEAD` branch is omitted
- a local row and a published row of one fixture get identical labels
- `TestFixtureOutputMatchesPublishedSchemas` passes

### PR 4 — scope and rows (~450 lines + tests)

`scopeFor` resolves the directory's repository key once, with a short
timeout, and falls back to paths. It applies to the picker, `list` (table,
browser, `--json` with `scope`), and title search.

- `a` toggles the scope, and the heading names it.
- `--all-projects` is added to `handoff` and `list`.
- `--project DIR|NAME` is added to `list`, and `handoff`'s applies beyond
  `--latest`.
- When nothing is in scope, the view falls back to all projects.
- The row changes are those in §7.
- Help, `cli_reference_test`, and the changelog's compatibility note are
  updated.

Tests:

- two worktrees of one repository are one scope (fake resolver)
- no origin falls back to `sameProject`
- outside any project, every session shows
- `--project` takes a directory or a name, and is refused with
  `--all-projects`
- `a` toggles in the browser, whichever command opened it
- an empty scope falls back and says so
- `list --json` carries `scope`
- columns are hidden when constant
- `●` follows `activeSourceWindow`
- goldens `testdata/browse/handoff-scoped.txt` and `list-scoped.txt`

### PR 5 — one matcher (~550 lines + tests)

- `sessionQuery`; `handoffQueryResolver`, `resolveShowQuery`, and `list` use
  the tiers of §3, and `matchSessionsByQuery` is removed.
- `show "<words>"` without a terminal prints the shared candidate table.
- `list` accepts a positional query, with help and `cli_reference_test`
  updated.
- `list`'s table and browser drop subagents before `--limit`, show the
  hidden count in the footer, and give parents the `· N subagents` hint (the
  picker gets the hint too).
- `handoff`'s candidate table gains the PR column and the `Next:` line.
- `internal/agentskills/skills/agent-archive/SKILL.md.tmpl` is updated:
  - words may be a topic, a PR number, a branch, or a project name
  - to see candidates as data, run `list "<words>" --json`
  - the "Never pick for them" rule stays

Tests:

- AND across fields
- `#N` and a bare number match PRs
- an in-scope match shadows others and the note counts them
- subagents only as the last tier
- `list` hides subagents but `list --json` keeps them
- `--limit` counts top-level rows
- `list "<words>" --json` keeps the document shape
- skill template golden

### PR 6 — one browser, filter as you type (~600 lines + tests)

`runBrowser` takes a `browserSpec` and backs every caller in the §5 table.
`runSessionBrowser` becomes its browse mode; `pickBrowseSession` and
`pickBrowseRow` are removed, and their callers pass a spec. The PR adds:

- `/`, the highlight, and Esc
- the line-mode filter
- `list "<words>"` opening with the filter filled in
- subagents indented under matching parents

A test lists every caller in the §5 table and checks that each reaches
`runBrowser`, so a new chooser cannot quietly grow its own loop. Tests:

- each caller in the §5 table opens the key browser on a terminal and the
  line-mode fallback without one, with the heading and Enter action its row
  gives
- key-mode filter narrows and Enter acts on the highlight, in browse and
  pick-one modes
- numbers keep their unfiltered values
- line-mode words filter and an empty answer clears
- a subagent match shows its parent
- a PTY test that a handoff picked in key mode still reads the destination
  prompt's typed-ahead answer
- goldens `keys-handoff-filtered.txt` and `keys-list-filtered.txt`

### PR 7 — subagent descriptions (~200 lines + goldens)

The collector passes the `.meta.json` beside a Claude subagent transcript to
the filter, which emits one `subagent-meta` record. Backfill's
`claudeSubagents` does the same for imported subagents. Tests:

- description kept and redacted
- other keys never kept
- a missing or malformed file changes nothing and records no gap (the file
  is optional)
- goldens regenerated

### PR 8 — docs and live check

Update the handoff guide, `list` in the CLI reference, and the agent-skills
guide. Live check on the owner's Mac:

- bare `show`, `list`, and `handoff` from inside agent-archive open the same
  browser on this repository with native names
- an ambiguous `show "flaky"` and `handoff "flaky"` open that browser too
- `a` shows all projects, and `list --project personal_website` shows that
  project
- `/linux` finds the Linux session from `show`, `list`, and `handoff`
- `/208` finds the PR-208 reviewer under its parent
- from Claude Code, "hand off my flaky retention test session to Codex"
  resolves without asking
- a Claude Code CLI session (not the desktop app) is checked for whether it
  writes `custom-title` without `/rename`, and the guide says what was seen

## Later

- Codex `thread_name` from `~/.codex/session_index.jsonl`. It is a second
  file per Codex home, keyed by thread ID, so it needs the same "file beside
  the transcript" handling as PR 7. Codex sessions are rare on the owner's
  Mac today.
- A highlight cursor in the unfiltered browser, so → could open a parent's
  subagents without searching.
- Searching the last prompt as a field. Claude's `last-prompt` record would
  need admitting, or the parser would need to keep the last human prompt.
- A configured default scope (always all projects), if `a` and
  `--all-projects` prove not enough.
