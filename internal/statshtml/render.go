// Package statshtml renders the numbers of `agent-archive stats` as one
// self-contained HTML page: inline styles and inline SVG, no script, no
// fonts and no request to anything else, so the file can be opened from
// disk, attached to a message or printed, and never phones anywhere.
//
// It is a pure function of the engine's stats.Stats and Options. It holds
// aggregates and names only: never prompts, transcript text, file paths or
// session IDs. Project, skill and MCP server names, and the names of models
// the built-in price table does not list, are replaced by stand-ins
// ("project A", "model A") unless Options.IncludeNames is set, and every name
// that came from a transcript (project, model, skill, MCP server) is cleaned
// of control characters and escaped for HTML by the template, which is the
// only way text reaches the page.
package statshtml

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	_ "embed" // the page template and stylesheet are compiled in

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

//go:embed page.html.tmpl
var pageTemplateSource string

//go:embed style.css
var styleSheet string

// pageTemplate is the page's template with the stylesheet set into its <style>
// element: the stylesheet is the template's own text, never a value passed in,
// so nothing is marked as trusted CSS.
var pageTemplate = template.Must(template.New("page").Parse(strings.Replace(pageTemplateSource, "/*STYLESHEET*/", styleSheet, 1)))

// contentSecurityPolicy is the page's own policy, in a meta tag: nothing may
// load from anywhere, no script may run, and only the page's inline styles
// apply. It is a second lock behind the escaping, so a name that somehow got
// through as markup still could not run or fetch anything.
const contentSecurityPolicy = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

// Row limits keep a page a few hundred kilobytes however large the archive:
// the engine bounds the daily series (stats.MaxDays), and these bound the
// tables.
const (
	maxModelRows   = 8
	maxGroupRows   = 60
	maxProjectRows = 25
	// maxBars is the most bars the daily chart draws; a longer window
	// shows the busiest day of each run of days.
	maxBars = 120
)

// Filters are the filters the stats run applied, named in the heading so the
// page says which sessions it covers. They are the caller's own flags.
type Filters struct {
	Harness string
	Model   string
	// Origin is "imported" or "hook" when only those sessions were counted.
	Origin string
}

// originText is how the heading names the Origin filters the command has.
var originText = map[string]string{
	"imported": "imported sessions",
	"hook":     "hook-captured sessions",
}

// Options say how to render.
type Options struct {
	// GeneratedAt is stamped in the footer; the zero time leaves the stamp
	// out. The renderer never reads the clock.
	GeneratedAt time.Time
	// IncludeNames shows the real names of projects, skills, MCP servers and
	// models the built-in price table does not list, which can identify a
	// client or an internal tool (a fine-tune id, a deployment name). By
	// default each is a stand-in ("project A", "skill A", "MCP server A",
	// "model A"), so the page can be shared. A model the built-in price table
	// lists ("opus", "gpt-5") is public and shown either way.
	IncludeNames bool
	// Filters are named in the heading.
	Filters Filters
	// EmptyMessage is shown when the window has no sessions; a default is
	// used when it is empty.
	EmptyMessage string
}

// Render returns the page for s.
func Render(s stats.Stats, opts Options) ([]byte, error) {
	b := builder{
		s: s, opts: opts,
		names:      newNamer(opts.IncludeNames, "project"),
		skills:     newNamer(opts.IncludeNames, "skill"),
		servers:    newNamer(opts.IncludeNames, "MCP server"),
		modelNames: newModelNamer(opts.IncludeNames, s.Models),
	}
	p := b.page()
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, p); err != nil {
		return nil, fmt.Errorf("render the stats page: %w", err)
	}
	return out.Bytes(), nil
}

// builder turns stats into the page's view model.
type builder struct {
	s    stats.Stats
	opts Options
	// names, skills and servers show the archive's project, skill and MCP
	// server names, or stand-ins for them.
	names   *namer
	skills  *namer
	servers *namer
	// models shows the model names the built-in price table lists, or
	// stand-ins for the others.
	modelNames *modelNamer
	cost       costFlags
}

func (b *builder) page() page {
	s := b.s
	window := fmt.Sprintf("Last %d days", s.Window.Days)
	if s.Window.Days == 1 {
		window = "Today"
	}
	p := page{
		CSP:   contentSecurityPolicy,
		Title: "agent-archive stats · " + strings.ToLower(window),
	}
	b.header(&p, window)
	if s.Coverage.Sessions == 0 {
		p.Empty = b.opts.EmptyMessage
		if p.Empty == "" {
			p.Empty = fmt.Sprintf("No archived sessions in the last %d days (%s to %s). Try a longer window, for example agent-archive stats --days 90 --html.",
				s.Window.Days, plain(s.Window.FirstDay), plain(s.Window.LastDay))
		}
		b.footer(&p)
		return p
	}
	p.Daily, p.NoTokens = b.daily()
	p.Cards, p.CardsVs = b.overview()
	p.Agents = b.agents()
	p.Models = b.models()
	p.Projects = b.projects()
	p.Tokens = b.tokens()
	p.Highlights = b.highlights()
	p.Groups = b.groups()
	b.footer(&p)
	return p
}

// header fills the title, subtitle and filters.
func (b *builder) header(p *page, window string) {
	s := b.s
	agents := "1 agent"
	if s.Coverage.Agents != 1 {
		agents = fmt.Sprintf("%d agents", s.Coverage.Agents)
	}
	sessions := plural(s.Coverage.Sessions, "session")
	if s.Coverage.Sessions != 1 {
		sessions = statsfmt.CommaInt(int64(s.Coverage.Sessions)) + " sessions"
	}
	if s.Coverage.SessionsWithTokens != s.Coverage.Sessions {
		sessions += fmt.Sprintf(" (%s with token data)", statsfmt.CommaInt(int64(s.Coverage.SessionsWithTokens)))
	}
	p.Subtitle = []string{window, plain(s.Window.FirstDay) + " to " + plain(s.Window.LastDay), agents, sessions}
	f := b.opts.Filters
	var parts []string
	if f.Harness != "" {
		parts = append(parts, "harness "+clean(f.Harness))
	}
	if f.Model != "" {
		parts = append(parts, "model "+b.modelNames.id(f.Model))
	}
	if f.Origin != "" {
		text, known := originText[f.Origin]
		if !known {
			text = clean(f.Origin) + " sessions"
		}
		parts = append(parts, text)
	}
	if len(parts) > 0 {
		p.Filters = "Filtered to " + strings.Join(parts, ", ") + "."
	}
}

func (b *builder) overview() ([]card, string) {
	o := b.s.Overview
	cost := "n/a"
	if o.Tokens.Value != nil {
		cost = b.cost.costText(b.s.Prices.Currency, o.Cost.Value, o.Cost.Approximate, o.Cost.Partial, false)
	}
	activeDays := 0
	if o.ActiveDays.Value != nil && finite(*o.ActiveDays.Value) {
		activeDays = int(*o.ActiveDays.Value + 0.5)
	}
	streak := ""
	switch {
	case o.CurrentStreak > 0:
		streak = fmt.Sprintf("streak %s (best %d)", plural(o.CurrentStreak, "day"), o.BestStreak)
	case o.BestStreak > 0:
		streak = fmt.Sprintf("no current streak (best %d)", o.BestStreak)
	}
	cards := []card{
		{Label: "Sessions", Value: measureCount(o.Sessions)},
		{Label: "Prompts", Value: measureCount(o.Prompts)},
		{Label: "Tokens", Value: measureTokens(o.Tokens)},
		{Label: "Est. cost", Value: cost, Note: costNote(b.s.Prices.AsOf)},
		{Label: "Active days", Value: fmt.Sprintf("%d/%d", activeDays, o.DaysInWindow), Note: streak},
	}
	measures := []stats.Measure{o.Sessions, o.Prompts, o.Tokens, o.Cost.Measure}
	vs := ""
	for i, m := range measures {
		cards[i].Delta, cards[i].DeltaSpoken = deltaText(m)
		if cards[i].Delta != "" {
			vs = fmt.Sprintf("vs the previous %d days", b.s.Window.Days)
			if b.s.Window.Days == 1 {
				vs = "vs the day before"
			}
		}
	}
	return cards, vs
}

// costNote says what the estimated cost rests on, where it is read: list
// prices, and how old they are.
func costNote(asOf string) string {
	if asOf == "" {
		return "at list price"
	}
	return "at list price, prices as of " + plain(asOf)
}

func (b *builder) agents() *barTable {
	t := &barTable{ID: "agents", Title: "Agents", Heading: "Agent", HasBars: true,
		Cols: []string{"Sessions", "Tokens", "Est. cost"}}
	for _, a := range b.s.Agents {
		tokens, cost := "unknown", "n/a"
		if a.Tokens != nil {
			tokens = statsfmt.TokenCount(*a.Tokens)
			cost = b.cost.costText(b.s.Prices.Currency, a.Cost.USD, a.Cost.Approximate, a.Cost.Partial, false)
		}
		t.Rows = append(t.Rows, barRow{
			Label: clean(a.Label), Pct: pct(a.SessionShare),
			Cells: []string{statsfmt.CommaInt(int64(a.Sessions)) + " (" + statsfmt.Percent(a.SessionShare) + ")", tokens, cost},
		})
	}
	t.BarNote = "Bars show each agent's share of sessions."
	return t
}

func (b *builder) models() *barTable {
	rows := b.s.Models
	if len(rows) == 0 {
		return nil
	}
	t := &barTable{ID: "models", Title: "Cost by model", Heading: "Model", HasBars: true,
		Cols: []string{"Est. cost", "Share"}}
	more := 0
	if len(rows) > maxModelRows {
		more = len(rows) - maxModelRows
		rows = rows[:maxModelRows]
	}
	for _, r := range rows {
		t.Rows = append(t.Rows, b.modelRow(r))
	}
	t.BarNote = "Bars show each model's share of the estimated cost."
	if more > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("and %d more models", more))
	}
	if b.modelNames.hidden {
		t.Notes = append(t.Notes, "Models the built-in price table does not list are replaced by letters in this file.")
	}
	return t
}

// modelRow is a model's row: its cost and share, or, for a model the price
// table does not list, its tokens and an empty bar (it has no share of the
// cost to draw).
func (b *builder) modelRow(r stats.ModelRow) barRow {
	label := b.modelNames.label(r.Label)
	if !r.Priced || r.Cost.USD == nil {
		return barRow{Label: label, Pct: "0%", Cells: []string{"unpriced", statsfmt.TokenCount(r.Tokens) + " tokens"}}
	}
	share := 0.0
	if r.CostShare != nil {
		share = *r.CostShare
	}
	cost := b.cost.costText(b.s.Prices.Currency, r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
	return barRow{Label: label, Pct: pct(share), Cells: []string{cost, statsfmt.Percent(share)}}
}

func (b *builder) projects() *barTable {
	rows := b.s.Projects
	if len(rows) == 0 {
		return nil
	}
	t := &barTable{ID: "projects", Title: "Top projects", Heading: "Project", HasBars: true,
		Cols: []string{"Sessions", "Tokens", "Est. cost"}}
	top := 0
	for _, r := range rows {
		top = max(top, r.Sessions)
	}
	for _, r := range rows {
		share := 0.0
		if top > 0 {
			share = float64(r.Sessions) / float64(top)
		}
		tokens, cost := "unknown", "n/a"
		if r.Tokens != nil {
			tokens = statsfmt.TokenCount(*r.Tokens)
			cost = b.cost.costText(b.s.Prices.Currency, r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
		}
		t.Rows = append(t.Rows, barRow{
			Label: b.names.project(r.Name), Pct: pct(share),
			Cells: []string{statsfmt.CommaInt(int64(r.Sessions)), tokens, cost},
		})
	}
	t.BarNote = "Bars show sessions."
	if more := b.s.TotalProjects - len(rows); more > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("and %d more projects", more))
	}
	if !b.opts.IncludeNames {
		t.Notes = append(t.Notes, "Project names are replaced by letters in this file.")
	}
	return t
}

func (b *builder) groups() *barTable {
	g := b.s.Groups
	if g == nil || len(g.Rows) == 0 {
		return nil
	}
	// By is one of the engine's few groupings; it is cleaned like any text.
	by := clean(string(g.By))
	title := "By " + by
	heading := "Group"
	if runes := []rune(by); len(runes) > 0 {
		heading = strings.ToUpper(string(runes[:1])) + string(runes[1:])
	}
	if g.By == stats.GroupWeek {
		title += " (weeks start Monday)"
	}
	rows, limit := g.Rows, maxGroupRows
	if g.By == stats.GroupProject {
		limit = maxProjectRows
	}
	t := &barTable{ID: "groups", Title: title, Heading: heading,
		Cols: []string{"Sessions", "Prompts", "Tokens", "Est. cost"}}
	if len(rows) > limit {
		if g.By == stats.GroupProject {
			t.Notes = append(t.Notes, fmt.Sprintf("and %d more projects", len(rows)-limit))
			rows = rows[:limit]
		} else {
			// Chronological rows: keep the newest.
			t.Notes = append(t.Notes, fmt.Sprintf("%d earlier rows are not shown", len(rows)-limit))
			rows = rows[len(rows)-limit:]
		}
	}
	for _, r := range rows {
		label := clean(r.Key)
		if g.By == stats.GroupProject {
			label = b.names.project(r.Key)
		}
		prompts, tokens, cost := "unknown", "unknown", "n/a"
		if r.Prompts != nil {
			prompts = statsfmt.CommaInt(*r.Prompts)
		}
		if r.Tokens != nil {
			tokens = statsfmt.TokenCount(*r.Tokens)
			cost = b.cost.costText(b.s.Prices.Currency, r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
		}
		t.Rows = append(t.Rows, barRow{Label: label, Cells: []string{statsfmt.CommaInt(int64(r.Sessions)), prompts, tokens, cost}})
	}
	return t
}

func (b *builder) highlights() []highlight {
	h := b.s.Highlights
	var out []highlight
	if h.BusiestDay != nil {
		out = append(out, highlight{"Busiest day", fmt.Sprintf("%s (%s)", dayLabel(h.BusiestDay.Date, b.s.Window.Days), plural(h.BusiestDay.Sessions, "session"))})
	}
	if h.FavoriteModel != nil {
		text := b.modelNames.label(h.FavoriteModel.Label)
		if h.FavoriteModel.By == "tokens" {
			text += " (by tokens)"
		}
		out = append(out, highlight{"Favorite model", text})
	}
	if c := h.CostliestSession; c != nil && c.Cost.USD != nil {
		text := b.cost.costText(b.s.Prices.Currency, c.Cost.USD, c.Cost.Approximate, c.Cost.Partial, true)
		if c.Project != "" {
			text += " · " + b.names.project(c.Project)
		}
		if drivers := driversText(c); drivers != "" {
			text += " (" + drivers + ")"
		}
		out = append(out, highlight{"Costliest session", text})
	}
	if t := h.ToolErrors; t != nil {
		text := fmt.Sprintf("%s of %s tool results flagged as errors, %s measured", statsfmt.RatePercent(t.Rate), statsfmt.CommaInt(t.Results), plural(t.Sessions, "session"))
		if t.UnknownSessions > 0 {
			text += fmt.Sprintf(" (%d do not record them)", t.UnknownSessions)
		}
		out = append(out, highlight{"Tool errors", text})
	}
	if m := h.MonthRank; m != nil && m.Of > 1 {
		of := fmt.Sprintf(" of the %d months with token data", m.Of)
		if m.Of >= stats.MonthsCompared {
			of = fmt.Sprintf(" of the last %d months", m.Of)
		}
		out = append(out, highlight{"Month rank", "This month so far is your " + ordinalHeaviest(m.Rank) + of})
	}
	return out
}

func (b *builder) footer(p *page) {
	s := b.s
	var lines []string
	scope := "Scope: this archive only."
	var unknown []string
	for _, a := range s.Agents {
		if a.UnknownTokenSessions > 0 {
			unknown = append(unknown, fmt.Sprintf("%s: %d", clean(a.Label), a.UnknownTokenSessions))
		}
	}
	if len(unknown) > 0 {
		scope += " Sessions with no token data are left out of token and cost totals (" + strings.Join(unknown, ", ") + ")."
	}
	lines = append(lines, scope)
	if s.Coverage.Sessions > 0 {
		lines = append(lines, joinSentences(
			fmt.Sprintf("Token data: %s of %s.", statsfmt.CommaInt(int64(s.Coverage.SessionsWithTokens)), plural(s.Coverage.Sessions, "session")),
			subagentNote(s.Coverage),
			fmt.Sprintf("Days are counted in %s; sessions are placed by when they were captured.", plain(s.Window.Timezone)),
		))
	}
	cost := "Cost is an estimate at list price, not a bill."
	if s.Prices.Version != "" {
		cost += fmt.Sprintf(" Prices %s, as of %s", plain(s.Prices.Version), plain(s.Prices.AsOf))
		if s.Prices.Overridden {
			cost += ", with your own price file applied"
		}
		cost += "."
	}
	lines = append(lines, cost)
	if b.cost.approx {
		lines = append(lines, fmt.Sprintf("~ priced at the session's main model for %s with no per-model split (metadata from before parser 0.14.0).",
			plural(s.Coverage.SessionsPricedAtMainModel, "session")))
	}
	if b.cost.partial {
		lines = append(lines, "+ leaves out tokens of models the price table does not list"+b.unpricedModels()+".")
	}
	p.Footer.Lines = lines
	p.Footer.Privacy = "This page holds counts and names only: no prompts, transcript text, file paths or session IDs."
	if !b.opts.IncludeNames {
		p.Footer.Privacy += " Project, skill, MCP server and unrecognized model names are replaced by letters; run with --include-names to show them."
	}
	if !b.opts.GeneratedAt.IsZero() {
		p.Footer.Generated = "Generated " + b.opts.GeneratedAt.Format("2006-01-02 15:04 MST") + " by agent-archive stats --html."
	}
}

func subagentNote(c stats.Coverage) string {
	if c.SubagentSessions == 0 {
		return ""
	}
	note := plural(c.SubagentSessions, "subagent session") + " counted with their parent sessions."
	if c.OrphanSubagents > 0 {
		note += fmt.Sprintf(" %d of them have no parent in the archive and count as sessions of their own.", c.OrphanSubagents)
	}
	return note
}

// unpricedModels lists, in parentheses, the models the window used that the
// price table does not list.
func (b *builder) unpricedModels() string {
	var names []string
	for _, r := range b.s.Models {
		if !r.Priced || r.Cost.USD == nil {
			names = append(names, b.modelNames.label(r.Label))
		}
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) > 3 {
		names = append(names[:3], "…")
	}
	return " (" + strings.Join(names, ", ") + ")"
}
