# See your usage: stats

`stats` shows how you use your coding agents: tokens by day, sessions,
estimated cost, which agents, models and projects took the most, what the
tokens were spent on, and a few highlights. It is read-only, like `list`: it
reads only the metadata of your archived sessions, so it prints numbers and
names (agents, models, projects, skills, MCP servers), never prompts,
transcript text or file paths.

```sh
agent-archive stats                        # the last 30 days, against the 30 before
agent-archive stats --days 7               # the last 7 days
agent-archive stats --since 2026-09-01     # from that local day through today
agent-archive stats --by week              # also break the window down by week
agent-archive stats --by project           # ...or by day, month, or project
agent-archive stats --harness claude       # one agent only
agent-archive stats --prices my-prices.json   # your own prices, see below
agent-archive stats --json                 # for scripts: see JSON output
agent-archive stats --html --output stats.html   # a web page you can share
```

The first run reads every session's metadata from the bucket, so it can take
a while on a large archive (a spinner shows on a terminal); it keeps a local
copy of what it read, as `list` does, and later runs only fetch what
changed. `--no-cache` reads everything again.

## What it shows

- **Tokens by day**, a small bar chart with the peak day.
- **Overview**: sessions, prompts, tokens and estimated cost, each with the
  change from the previous period of the same length, and the days you were
  active, with your current and best streak.
- **Agents**, **cost by model** and **top projects**. A project is the
  project's name only.
- **What used your tokens**: cache reads, cache writes, fresh input and
  output. Rows appear only when there is something to show: how much of your
  tokens subagents used, the skills sessions used (counted in *sessions that
  used each one*, not calls), and MCP servers (counted in calls).
- **Highlights**: your busiest day, favorite model, the costliest session
  and what likely made it costly (long context, subagents, a low cache hit
  rate), the share of tool results the app flagged as errors, and how this
  month ranks against your last six.

`--by day|week|month|project` adds a table of the window broken down that
way (weeks start on Monday). On a terminal of 100 columns or more the screen
draws bars; narrower, it is a compact table. It is paged through `$PAGER` on a
terminal (`--no-pager` to print directly), plain text when piped, and ASCII
instead of block characters in a locale that is not UTF-8.

## Share it as a web page

```sh
agent-archive stats --html --output stats.html   # then open stats.html
agent-archive stats --days 90 --html > q3.html   # or redirect standard output
```

`--html` writes the same numbers as one web page you can open in a browser,
attach to a message, or print. It is a single file: the styles and charts are
inline, there is no script, and it makes no request to anything (its own
content policy forbids it), so it works offline and reads the same
tomorrow. It follows your light or dark setting, prints cleanly (the donut
switches to patterns, so it survives a black-and-white printer), and fits a
phone. Every bar answers a hover with its exact numbers, and the daily chart
has a "Show as a table" view for screen readers.

- **Where it goes.** `--output FILE` saves it with mode 0600 (readable by you
  only; change that when you share it) and says so on stderr. Without
  `--output` it goes to standard output, so redirect it; on a terminal
  `--html` alone asks you for `--output` rather than filling the screen with
  markup. `--output` never replaces a file that exists unless you add
  `--force`, and it is checked before the archive is read. `--html` cannot be
  combined with `--json`.
- **What is in it.** Counts, model names, skill and MCP server names, dates,
  and project names, only. Never a prompt, transcript text, a file path or a
  session ID (the costliest session is described, not named: `show` cannot
  open it from the page, by design).
- **Project names are hidden by default.** So the page can be shared, each
  project is a stand-in, "project A", "project B" and so on, in the order the
  page lists them: the same project has the same letter throughout the page,
  but the letters follow this run's ranking and are not stable between runs.
  `--include-project-names` shows the real names, for a page only you read.
  Skill and MCP server names are shown as they are: they are names of tools,
  but a skill or server you named after a client would be visible, so check
  those before sharing.
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
  and cost read `unknown` / `n/a` and are left out of the totals; the heading
  and the footer say how many sessions that is. Codex does not record whether
  a tool call failed, so tool errors cover the other sessions, and the
  highlight says how many. Sessions captured before parser `0.14.0` lack the
  per-model split; they are priced at their main model (marked `~`) until
  their metadata refreshes.
- **Cost is an estimate**, at list price from a dated price table built into
  the release (the footer names its version and date), not a bill. Prices for
  a model the table does not list are left out rather than guessed; the
  total is then marked `+` and the footer names the models. Reasoning tokens
  cost what output costs. Cache writes use the five-minute rate.
- **`--prices FILE`** puts your own entries on top of the built-in table, in
  the same JSON shape (`internal/stats/prices.json` in the source shows it):
  a `version`, an `as_of` date, and `models` with `id`, and the four prices
  per million tokens (`input_per_mtok`, `output_per_mtok`,
  `cache_read_per_mtok`, `cache_write_per_mtok`; `0` for a token type that
  costs nothing). The output says your prices were applied.
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
