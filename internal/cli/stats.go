package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// statsSchemaVersion versions the `stats --json` document.
const statsSchemaVersion = 1

// maxPricesFileBytes bounds a --prices file: a price table is a few
// kilobytes, and nothing else should be read as one.
const maxPricesFileBytes = 1 << 20

// statsDocument is what `stats --json` prints: the engine's Stats, with its
// fields at the top level, under a schema version and the filters that
// narrowed the sessions. It holds counts, model names, project names, skill
// and MCP server names and one session ID: never prompts, transcript text or
// paths. A null is unknown, never zero; see stats.Stats.
type statsDocument struct {
	Version     int          `json:"schema_version"`
	GeneratedAt time.Time    `json:"generated_at"`
	Filters     statsFilters `json:"filters"`
	stats.Stats
}

// statsFilters echoes the filters a stats run applied, so a document says
// which sessions it covers. A filter that was not set is left out.
type statsFilters struct {
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	// Origin is "imported" or "hook" when --imported or --hook-captured was
	// given.
	Origin string `json:"origin,omitempty"`
}

// runStatsCommand implements `agent-archive stats`. Like list, it reads only
// metadata sidecars (never a source bundle), reuses the local metadata cache
// unless --no-cache, and prints only numbers, model names, project names and
// the names of skills and MCP servers, so its output can never contain
// prompts, transcript text or paths.
//
// The window is calendar days in the machine's time zone ending today:
// --days N (default 30), or from the local day --since names through today.
// The previous period of the same length and the last six months are read
// too, for the overview's changes and the month rank, but only the window is
// reported.
func runStatsCommand(args []string, stdout, stderr io.Writer, env statsCommandDependencies) int {
	fs := env.newCommandFlags("stats", stderr)
	harness := fs.String("harness", "", "only sessions from this harness (codex, claude, cursor)")
	model := fs.String("model", "", "only sessions that requested or observed this model")
	since := fs.String("since", "", "start the window on this local day: a date (2026-09-01), RFC 3339 time, or age (7d, 12h)")
	days := fs.Int("days", stats.DefaultDays, "the window's length in calendar days, ending today")
	by := fs.String("by", "", "also break the window down by day, week, month, or project")
	pricesFile := fs.String("prices", "", "price the tokens from this JSON file's prices on top of the built-in table")
	imported := fs.Bool("imported", false, "only sessions agent-archive backfill imported")
	hookCaptured := fs.Bool("hook-captured", false, "only sessions captured by hooks as they ran")
	noCache := fs.Bool("no-cache", false, "download every metadata sidecar instead of reusing unchanged ones from the local metadata cache")
	noPager := fs.Bool("no-pager", false, "print directly to the terminal; do not page through $PAGER")
	jsonOut := fs.Bool("json", false, "print a versioned JSON document of the numbers")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	daysSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "days" {
			daysSet = true
		}
	})
	loc := statsZone(env.now(), env)
	now := env.now().In(loc)
	windowDays, code := statsWindowDays(fs, *days, daysSet, *since, now)
	if code != 0 {
		return code
	}
	grouping := stats.GroupNone
	if *by != "" {
		g, ok := stats.ParseGrouping(*by)
		if !ok {
			return fs.usageError("--by must be day, week, month, or project, not %q", *by)
		}
		grouping = g
	}
	table, code := statsPriceTable(fs, *pricesFile)
	if code != 0 {
		return code
	}
	// listOptionsFromFlags checks --harness and --model as list does. Its
	// --since is list's (midnight UTC); stats reads a date as a local day, so
	// it is not passed and the window sets the filter's From below.
	opts, code := listOptionsFromFlags(fs, listFlagValues{
		harness: *harness, model: *model, skillUsage: string(reader.SkillUsageUsed),
		imported: *imported, hookCaptured: *hookCaptured, noCache: *noCache,
	}, now)
	if code != 0 {
		return code
	}
	opts.filter.From = statsFetchFrom(now, loc, windowDays)

	store, _, found, err := openReadOnlyStore(env)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	stopReading := func() {}
	if !*jsonOut {
		stopReading = startActivity(stdout, "Reading sessions…")
	}
	listed, err := reader.ListRecent(context.Background(), store, archiveSessionsPrefix, opts.filter, 0,
		reader.ListOptions{Cache: listCache(env, opts.noCache), Skipped: warnSkippedSidecar(stderr, "stats")})
	stopReading()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	sessions := filterListOrigin(listed.Sessions, opts.imported, opts.hookCaptured)
	computed := stats.Compute(sessions, stats.Options{
		Now: now, Days: windowDays, Location: loc, PriceTable: table, By: grouping,
	})
	filters := statsFilters{Harness: opts.filter.Harness, Model: opts.filter.Model}
	switch {
	case opts.imported:
		filters.Origin = "imported"
	case opts.hookCaptured:
		filters.Origin = "hook"
	}
	if *jsonOut {
		return printJSON(stdout, stderr, statsDocument{
			Version: statsSchemaVersion, GeneratedAt: now, Filters: filters, Stats: computed,
		})
	}
	if computed.Coverage.Sessions == 0 {
		terminal.Println(stdout, newStatsView(stdout, env).wrap(statsEmptyMessage(computed, filters, len(sessions) > 0)))
		return 0
	}
	view := newStatsView(stdout, env)
	view.filters = filters
	if err := withPager(context.Background(), stdout, stderr, env, *noPager, func(w io.Writer) error {
		return renderStats(w, computed, view)
	}); err != nil {
		terminal.Printf(stderr, "agent-archive: stats: %v\n", err)
		return 1
	}
	return 0
}

// statsWindowDays is the window's length in calendar days: --days, or the
// days from the local day --since names through today, so the window is
// whole days and --since can never disagree with what the engine counts. A
// time or age selects from the start of its local day, as backfill's --since
// does. It returns a usage error's exit code when the flags conflict or are
// out of range.
func statsWindowDays(fs *commandFlags, days int, daysSet bool, since string, now time.Time) (int, int) {
	if since != "" && daysSet {
		return 0, fs.usageError("choose one of --days and --since")
	}
	if since == "" {
		if days < 1 || days > stats.MaxDays {
			return 0, fs.usageError("--days must be from 1 to %d", stats.MaxDays)
		}
		return days, 0
	}
	from, err := parseTimeArg(since, now, now.Location())
	if err != nil {
		return 0, fs.usageError("--since: %v", err)
	}
	span := calendarDaysBetween(from.In(now.Location()), now) + 1
	if span < 1 {
		return 0, fs.usageError("--since is in the future")
	}
	if span > stats.MaxDays {
		return 0, fs.usageError("--since reaches back %d days; the most is %d (--days)", span, stats.MaxDays)
	}
	return span, 0
}

// calendarDaysBetween is how many calendar days later b's date is than a's,
// both read in b's zone. It counts dates, never 24-hour spans, so a day with
// a daylight-saving change is still one day.
func calendarDaysBetween(a, b time.Time) int {
	ay, am, ad := a.In(b.Location()).Date()
	by, bm, bd := b.Date()
	start := time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)
	end := time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC)
	return int(end.Sub(start).Hours() / 24)
}

// statsFetchFrom is the earliest capture time stats reads: the start of the
// previous period (as long as the window before it), or of the month five
// before this one for the month rank, whichever is earlier.
func statsFetchFrom(now time.Time, loc *time.Location, windowDays int) time.Time {
	year, month, day := now.In(loc).Date()
	previous := time.Date(year, month, day-(2*windowDays-1), 0, 0, 0, 0, loc)
	rank := time.Date(year, month-time.Month(stats.MonthsCompared-1), 1, 0, 0, 0, 0, loc)
	if rank.Before(previous) {
		return rank
	}
	return previous
}

// statsPriceTable is the price table to cost with: the built-in one, or with
// the entries of the --prices file applied on top. A file that cannot be
// read or is not a price table is a usage error, reported before any
// storage is touched.
func statsPriceTable(fs *commandFlags, path string) (stats.PriceTable, int) {
	if path == "" {
		return stats.PriceTable{}, 0
	}
	file, err := os.Open(path)
	if err != nil {
		return stats.PriceTable{}, fs.usageError("--prices: %v", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxPricesFileBytes+1))
	if err != nil {
		return stats.PriceTable{}, fs.usageError("--prices: %v", err)
	}
	if len(data) > maxPricesFileBytes {
		return stats.PriceTable{}, fs.usageError("--prices: the file is larger than %d bytes, so it is not a price table", maxPricesFileBytes)
	}
	custom, err := stats.ParsePriceTable(data)
	if err != nil {
		return stats.PriceTable{}, fs.usageError("--prices: %v", err)
	}
	return stats.DefaultPriceTable().WithOverrides(custom), 0
}

// statsEmptyMessage says why there is nothing to show, and what to try.
// sawSessions is whether any session (of the previous period, say) was read.
func statsEmptyMessage(s stats.Stats, filters statsFilters, sawSessions bool) string {
	span := fmt.Sprintf("in the last %d days (%s to %s)", s.Window.Days, s.Window.FirstDay, s.Window.LastDay)
	if s.Window.Days == 1 {
		span = fmt.Sprintf("today (%s)", s.Window.LastDay)
	}
	longer := ""
	switch {
	case s.Window.Days < 90:
		longer = " Try a longer window, for example agent-archive stats --days 90."
	case s.Window.Days < 365:
		longer = " Try a longer window, for example agent-archive stats --days 365."
	}
	switch {
	case filters != statsFilters{}:
		return "No archived sessions match these filters " + span + "." + longer
	case sawSessions:
		return "No archived sessions were captured " + span + "." + longer
	}
	return "No archived sessions " + span + ". If you have just set up, sessions appear once an app session is captured (agent-archive status shows capture)." + longer
}

// statsZone is the time zone days, weeks and months are counted in: the
// clock's own. The machine's local zone is found by its name, from $TZ or
// where /etc/localtime points, so the output can say which zone it counted
// in (Go's time.Local is only called "Local"); when no name matches the
// clock's offset, it stays time.Local.
func statsZone(now time.Time, env interface{ lookupEnv(string) (string, bool) }) *time.Location {
	if now.Location() != time.Local {
		return now.Location()
	}
	var names []string
	if tz, _ := env.lookupEnv("TZ"); tz != "" {
		names = append(names, strings.TrimPrefix(tz, ":"))
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, found := strings.Cut(target, "zoneinfo/"); found {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if name == "" || name == "Local" {
			continue
		}
		if named, err := time.LoadLocation(name); err == nil && now.In(named).Format("-07:00") == now.Format("-07:00") {
			return named
		}
	}
	return time.Local
}
