# See your usage: stats

`stats` shows how you use your coding agents. The default screen is a short
summary of the last 30 days: what it cost, how many sessions, how many tokens,
which agents did the work, what each day cost, where it went by project and
model, the skills and MCP servers used most, and anything worth a second
look. More is one flag away: `--detail` for the full breakdown, and
`--view projects`, `models` or `agents` to list every one. It is read-only,
like `list`: it reads only the metadata of your archived sessions, so it
prints numbers and names (agents, models, projects, skills, MCP servers),
never prompts, transcript text or file paths.

```sh
agent-archive stats                        # the summary of the last 30 days
agent-archive stats --detail               # every number, and what they rest on
agent-archive stats --view projects        # every project, by spend (also models, agents)
agent-archive stats --days 7               # the last 7 days
agent-archive stats --since 2026-09-01     # from that local day through today
agent-archive stats --by week              # the detail screen, plus a table by week
agent-archive stats --harness claude       # one agent only
agent-archive stats --prices my-prices.json   # your own prices, see below
agent-archive stats --json                 # for scripts: see JSON output
agent-archive stats --html --output stats.html   # a web page you can share
```

The first run reads every session's metadata from the bucket, so it can take
a while on a large archive (a spinner with a count shows on a terminal, and
Ctrl-C stops it); it keeps a local
copy of what it read, as `list` does, and later runs only fetch what
changed. `--no-cache` reads everything again.

## What it shows

`--view` picks a screen (`overview` is the default); `--detail` is
`--view detail`. Every screen is plain text with color where the terminal has
it, and prints the same numbers to a pipe.

### The summary

```text
agent-archive stats · last 30 days · 3 agents

  ~$3,989                   93 sessions               10B tokens
  at list price             673 prompts               97% served from cache

AGENTS  ███████████████████████████████████████████ ████ █
        ● Claude Code 90%   ● Cursor 9%   ● Codex 1%   of sessions

DAILY SPEND                           peak ~$2,910 · Sep 27
                                                      █
                                                      █
▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▂▁█▁▁▁▇
Aug 31                                               Sep 29

WHERE IT WENT
By project                                By model
agent-archive  ███████████████  $1,862    opus    ██████████████████████  $3,270
levenshtein    ██████             $751    fable   ███                       $386
styleprofile   █████              $586    sonnet  ██                        $320
family_books   ███                $427    + 2 more
+ 6 more

MOST USED
Skills  code-review 10 · review-pr 4 · docs 3 · cursor-guide 2 sessions
MCP     github 41 · linear 12 calls (Claude Code and Cursor only)

HEADS UP
● 77% of tokens came from subagents (497 runs)
● Costliest session ~$564 · styleprofile · long context, 38 subagents
● 10 sessions have no token data (Cursor 8, Claude Code 2)

Estimated at list price, not a bill.   --detail for more · --by project · --html
```

- **The headline** is estimated spend, sessions and tokens. When the period
  before this one had any, spend shows its change (`▲ 18% vs prior 30d`):
  an arrow up is amber and down is green, never red, because more spend is
  not an error. A change of more than 999% reads `▲ >999%` (against next to
  nothing the exact figure only measures how little there was; `--json` has it
  exactly). With nothing to compare against, nothing is shown, never a
  "new". Under the tokens is how much of them were cache reads: most of a long
  session's tokens are the same context read again, so the count alone
  overstates the work.
- **Agents** is one bar split by each agent's share of your sessions.
- **Daily spend** is the estimated cost of each day, three rows tall, with
  the peak day named. A day with no sessions is only the baseline, and a day
  whose sessions have no price (Cursor records no tokens) is a dot, never a
  low bar (a line under the chart says so). When the window has more days than
  the terminal has columns, as 90 days do on 80, each bar is the costliest day
  of a run of days, and a line under the chart says how many. A window of one
  day has no chart.
- **Where it went** is the projects and the models by spend. From 80 columns
  they are two columns; from 60 they are stacked; narrower, plain rows. Bars
  are scaled to the largest row of their list and have no track behind them.
- **Most used** lists skills (in *sessions that used each one*, not calls; a
  plugin's prefix is dropped, so `anthropic-skills:docs` reads `docs`) and
  MCP servers (in calls, for the agents that record them). A row appears only
  when there is data for it.
- **Heads up** is up to three things worth a look, in this order: subagents
  using a quarter or more of your tokens (a subagent run is counted as a run,
  never as a session), one session costing a tenth or more of your spend (with
  more than one session in the window) and what likely made it costly, sessions with no token data, and a cache hit
  rate under 60%.

The colors are the terminal's own 16 (so they follow your theme): Claude Code
yellow, Cursor blue, Codex green; models by family (opus magenta, fable red,
sonnet cyan, haiku yellow, GPT and Codex models green); projects and the
daily chart cyan. Color is never the only cue: every legend names what it
colors. `NO_COLOR` turns color off, and so does anything that is not a
terminal.

### The detail screen (`--detail`)

Everything the summary leaves out: each number against the previous period,
the agents table (with cache hit rate), the daily chart, what used your
tokens (cache reads and writes, fresh input and output, and how much subagents
used), highlights (days active and streaks, the busiest day, your favorite
model, how this month ranks against your last six, the share of tool results
the app flagged as errors, the costliest session), every skill and MCP server,
and notes on what the numbers rest on. `--by day|week|month` adds a table of
the window broken down that way (weeks start on Monday) under it.

```text
agent-archive stats · detail · last 30 days · 3 agents

OVERVIEW     last 30d  prior 30d     change
Est. spend    ~$3,989    ~$3,380      ▲ 18%
Sessions           93        121      ▼ 23%
Prompts           673        673  no change
Tokens            10B       8.5B      ▲ 18%
Active days   3 of 30    5 of 30      ▼ 40%
97% of tokens were served from cache.

AGENTS         sessions  share   tokens  est. cost  cache hit
● Claude Code        84    90%     9.9B     $3,940        97%
● Cursor              8     9%  unknown        n/a        n/a
● Codex               1     1%     100M        $49        90%

DAILY SPEND                           peak ~$2,910 · Sep 27
                                                      █
                                                      █
▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▂▁█▁▁▁▇
Aug 31                                               Sep 29

WHAT USED YOUR TOKENS
████████████████████████████████████████████ █ █ █
● Cache read    97%  9.7B
● Cache write    2%  200M
● Fresh input   <1%  30M
● Output         1%  70M
Subagents  77% of tokens (7.7B) in 497 runs

HIGHLIGHTS
Streak          none now (best 1 day)
Busiest day     Sep 27 (70 sessions)
Favorite model  opus
This month      the heaviest of the last 6 months, by tokens so far
Tool errors     5.0% of 4,200 tool results flagged as errors, 70 sessions
                measured (13 do not record them)
Costliest       ~$564 · styleprofile · long context, 38 subagents

SKILLS AND MCP
Skills  code-review 10 · review-pr 4 · docs 3 · cursor-guide 2 sessions
MCP     github 41 · linear 12 calls (Claude Code and Cursor only)
Skills count the sessions that used each one; MCP counts calls.

HEADS UP
● 77% of tokens came from subagents (497 runs)
● Costliest session ~$564 · styleprofile · long context, 38 subagents
● 10 sessions have no token data (Cursor 8, Claude Code 2)

NOTES
Scope: this archive only.
83 of 93 sessions report token counts.
Sessions with no token data are left out of token and cost totals (Claude Code:
2, Cursor: 8).
Prices 2026-09-29, as of 2026-09-29.
~ marks an estimate at list price.
MCP: Claude Code and Cursor only; Codex MCP calls are not recorded.

Estimated at list price, not a bill.   --view overview
```

### Projects, models and agents

`--view projects` lists every project with a bar, its sessions, tokens, spend
and share of the spend (a project is the project's name only). `--view models`
lists every model family, with any model the price table does not list flagged
`unpriced` and its tokens shown. `--view agents` is the agents table, with each
agent's share of sessions, tokens, spend and cache hit rate, and a note on
what each agent does not record. `--by project` is `--view projects`.

```text
agent-archive stats · projects · last 30 days · 3 agents

PROJECTS (10)                        sessions   tokens  est. cost  share
agent-archive  ████████████████████        31     4.6B     $1,862    47%
levenshtein    ████████                    18     1.9B       $751    19%
styleprofile   ██████                       9     1.5B       $586    15%
family_books   █████                       12     1.1B       $427    11%
benchplan      ██                           6     500M       $200     5%
notes          █                            5     170M        $70     2%
blog           █                            4     110M        $45     1%
scripts        █                            3      70M        $28     1%
infra          █                            3      50M        $20     1%
dotfiles                                    2  unknown        n/a

Estimated at list price, not a bill.   --view overview
```

```text
agent-archive stats · models · last 30 days · 3 agents

MODELS (5)                     sessions  tokens  est. cost  share
opus     ████████████████████        60      7B     $3,270    82%
fable    ██                          20    1.5B       $386    10%
sonnet   ██                          30    1.3B       $320     8%
gpt-5.6  █                            1    100M      $7.00    <1%
haiku    █                           10    100M      $6.00    <1%

Estimated at list price, not a bill.   --view overview
```

```text
agent-archive stats · agents · last 30 days · 3 agents

AGENTS                            sessions  share   tokens  est. cost  cache hit
● Claude Code  ███████████████          84    90%     9.9B     $3,940        97%
● Cursor       █                         8     9%  unknown        n/a        n/a
● Codex        █                         1     1%     100M        $49        90%

Claude Code: 2 of 84 sessions have no token data; left out of tokens and spend.
Cursor: 8 of 8 sessions have no token data; left out of tokens and spend.
Codex: MCP calls and tool errors are not recorded.
Cache hit is cache reads over all input-side tokens.

Estimated at list price, not a bill.   --view overview
```

Lists are cut at 500 rows, and say how many more there are: `--json` has them
all.

### Narrow terminals

Two columns of bars from 80 columns, one column from 60, and below that the
same numbers as plain rows, down to 40 columns; nothing ever runs past the
edge. A window whose spend has a previous period looks like this at 60
columns:

```text
agent-archive stats · last 30 days · 3 agents

  ~$3,989              93 sessions   10B tokens
  ▲ 18% vs prior 30d   673 prompts   97% served from cache

AGENTS  █████████████████████████ ██ █
        ● Claude Code 90%   ● Cursor 9%   ● Codex 1%
        of sessions

DAILY SPEND                           peak ~$2,910 · Sep 27
                                                      █
                                                      █
▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▂▁█▁▁▁▇
Aug 31                                               Sep 29

WHERE IT WENT
By project
agent-archive  ██████████████████████████████  $1,862
levenshtein    ████████████                      $751
styleprofile   █████████                         $586
family_books   ███████                           $427
+ 6 more

By model
opus           ██████████████████████████████  $3,270
fable          ████                              $386
sonnet         ███                               $320
+ 2 more

MOST USED
Skills  code-review 10 · review-pr 4 · docs 3 · cursor-guide
        2 sessions
MCP     github 41 · linear 12 calls (Claude Code and Cursor
        only)

HEADS UP
● 77% of tokens came from subagents (497 runs)
● Costliest session ~$564 · styleprofile · long context, 38
  subagents
● 10 sessions have no token data (Cursor 8, Claude Code 2)

Estimated at list price, not a bill.
--detail for more · --by project · --html
```

The screen is paged through `$PAGER` on a terminal (`--no-pager` to print
directly), plain text when piped, and ASCII instead of block characters in a
locale that is not UTF-8.

## In `--json`: spend by day and heads-up notes

`--json` carries the numbers the screen and the page are built from, so a
script can chart or check them: each `daily` entry has that day's estimated
`cost` (the days add up to the overview's cost), `peak_spend` names the
dearest day, `overview.cache_share` is the part of your tokens that were
cache reads, and `heads_up` lists up to three things worth a second look
(subagents using a quarter or more of your tokens, one session costing a tenth
or more of your spend when the window has more than one session, sessions with no token data, a low cache-hit rate) as
data, in that order of priority. A skill that a plugin provides is listed once
as `docs` in `display_skills` however it was recorded (`anthropic-skills:docs`),
and `skills` keeps the recorded names. Every field, rule and threshold is in
[JSON output](../reference/json-output.md#stats---json).

## Share it as a web page

```sh
agent-archive stats --html --output stats.html   # then open stats.html
agent-archive stats --days 90 --html > q3.html   # or redirect standard output
```

`--html` writes the same numbers as one web page you can open in a browser,
attach to a message, or print. It is a single file: the styles and charts are
inline, there is no script, and it makes no request to anything (its own
content policy forbids it), so it works offline and reads the same
tomorrow. It follows your light or dark setting, prints cleanly (the agents
bar and the donut switch to patterns, so they survive a black-and-white
printer), and fits a phone down to 320 pixels wide.

The page has the shape of the terminal's default view. At the top are the
three headline numbers: estimated spend (with its change from the previous
period, when there was one; nothing is said when there was not), sessions
(with the prompts under it) and tokens (with the share that was served from
cache, since most tokens are cache reads). Then the agents as one bar with
each agent's share of sessions, daily **spend** as a bar chart with its
dearest day named (a day whose sessions could not be priced gets a short grey
mark, not a zero), where the spend went by project and by model, the skills
and MCP servers used most, and up to three things worth a second look ("Heads
up": subagents using a large share of your tokens, one session costing much of
the spend, sessions with no token data, a low cache-hit rate). Under a
"Details" divider come the agents' table (sessions, tokens, spend, cache-hit
rate), what used your tokens (the donut), a few facts (days active and
streaks, busiest day, favorite model, costliest session, tool errors, month
rank), the `--by` breakdown, and the scope and price notes.

The colors mean the same as in the terminal: Claude Code orange, Cursor blue,
Codex green; model families purple (opus), pink (fable), teal (sonnet),
yellow (haiku) and green (gpt and codex models); projects and the daily bars
cyan; a rise in spend amber and a fall green, never red. A color is never the
only cue: every segment and bar is named and numbered in text. Each bar of the
daily chart answers a hover with that day's numbers, and the chart has a "Show
as a table" view (sessions, spend and tokens per day) for screen readers and
keyboards; every other number is in a table or a sentence.

The by-project list shows only the top few projects, dearest first among those
shown, and says how many more there are (it does not say they cost less or
more); `--by project` lists more of them.

- **Where it goes.** `--output FILE` saves it with mode 0600 (readable by you
  only; change that when you share it) and says so on stderr. The page is
  written to a temporary file next to it and moved into place, so a failure
  never leaves half a page, and it does not replace a file that exists unless
  you add `--force`. A symbolic link, a folder or a device is never written or
  replaced, `--force` or not. Without `--output` it goes to standard output,
  so redirect it (a failed write is an error, exit 1); on a terminal `--html`
  alone asks you for `--output` rather than filling the screen with markup.
  The flags are checked before the archive is read. `--html` cannot be
  combined with `--json`.
- **What is in it.** Counts, model names, skill and MCP server names, dates,
  and project names, only (by default only the model names the built-in
  price table lists, see the next item). Never a prompt, transcript text, a
  file path or a session ID (the costliest session is described, not named: `show` cannot
  open it from the page, by design). It also names the time zone the days were
  counted in. Subagents are counted as runs ("497 runs"), never as sessions.
- **Names are hidden by default.** So the page can be shared, each project is
  a stand-in, "project A", "project B" and so on, in the order the page lists
  them; skills, MCP servers and models get "skill A", "MCP server A" and
  "model A" the same way. A model keeps its name only when the built-in price
  table lists it ("opus", "gpt-5"; so are the archive's own "unknown" and
  "other"); a fine-tune id, a custom deployment name or any other model the
  table does not list is a stand-in, in the heading's `--model` filter, the
  by-model table, the favorite model and the footer's note on unpriced
  models alike (and it is drawn in a neutral color, so the color of a bar
  says nothing about a hidden model). A model your own `--prices` file adds
  is not listed either, and neither is the version of your own `--prices`
  file (the footer says only that the file was used). A `--model` filter
  written with a path, a vendor prefix or a bracketed suffix
  (`acme/claude-opus-5`, `claude-opus-5[acme]`) is shown as the model's family, not
  as typed. The project of the costliest session in "Heads up" is
  a stand-in like any other. The same name has the same letter throughout the
  page, but the letters follow the order the page first names them (the
  dearest project listed is "project A") and are not stable between runs. `--include-names` shows the real names of all four, for a page only
  you read. The terminal view and `--json` are not affected: they print to you.
- **Same rules as the terminal.** Unknown is not zero (Cursor's tokens read
  unknown, and a window with no token data says so instead of drawing an empty
  chart), cost is an estimate whose price table and date are in the footer,
  and the filters (`--harness`, `--model`, `--imported`, `--hook-captured`),
  `--days`, `--since`, `--prices` and `--by` apply. A window with no sessions
  gets a page that says why and what to try.
- **Size.** A page is tens of kilobytes. A long window draws at most 120 bars
  (each the busiest day of its run of days) and the tables are cut to a
  fixed number of rows, so even a very large archive gives a small file.

## Reading the numbers

- **Unknown is not zero.** Cursor records no token counts, so Cursor's tokens
  and cost read `unknown` / `n/a` and are left out of the totals; a heads-up
  line and the detail notes say how many sessions that is. Codex does not record whether
  a tool call failed, so tool errors cover the other sessions, and the
  highlight says how many. Sessions captured before parser `0.14.0` lack the
  per-model split; they are priced at their main model until their metadata
  refreshes, and the detail notes count them.
- **Cost is an estimate** (a `~` before an amount says so), at list price from
  a dated price table built into the release (the detail notes name its
  version and date), not a bill. Prices for
  a model the table does not list are left out rather than guessed; the
  total is then marked `+` and the detail notes name the models. Reasoning tokens
  cost what output costs. Cache writes use the five-minute rate.
- **`--prices FILE`** puts your own entries on top of the built-in table, in
  the same JSON shape (`internal/stats/prices.json` in the source shows it):
  a `version` (up to 64 bytes), an `as_of` date, an optional `currency`
  (a three-letter code such as `EUR`; default `USD`; a file in another
  currency replaces the built-in prices rather than mixing with them), and
  `models` with `id`, and the four prices per million tokens
  (`input_per_mtok`, `output_per_mtok`, `cache_read_per_mtok`,
  `cache_write_per_mtok`; `0` for a token type that costs nothing). The
  output says your prices were applied.
- **Days and time.** The window is whole calendar days in your time zone,
  ending today. `--since` starts it on the local day it names (a date is a
  local day here, unlike `list --since`, which reads it as UTC), so
  `--since 7d` is today and the seven days before it. `--days` and
  `--since` cannot be combined.
- **Capture time, not start time.** Sessions are placed by when they were
  captured, as `list --since` does, so a session you imported with
  `backfill` shows on the day it was imported. `--hook-captured` leaves
  imports out; `--imported` shows only them.
- **Subagents.** A subagent's tokens and cost count with its parent session,
  which is one session; the share they used is shown separately.
- **Scope.** This archive only: every session in your bucket, including
  those from other Macs that share it, and nothing that was never captured.
- **MCP.** Claude Code and Cursor only; Codex MCP calls are not recorded.
- **Month rank** compares this month so far with the five months before it,
  so early in a month it reads low.

`--harness`, `--model`, `--imported` and `--hook-captured` narrow the
sessions counted, the previous period included. `--model` keeps a session that
used the model, and counts all of that session's tokens, models included.
Give the full model id (`claude-opus-5`), as `list` does, not the family the
screen groups it under (`opus`).
