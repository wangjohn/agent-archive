package cli

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Layout constants for the stats screen.
const (
	// statsFullWidth is the terminal width from which the screen draws bars
	// (the designed screen, which is laid out for 80 columns); below it, it
	// is a compact table.
	statsFullWidth = 80
	// statsMaxWidth is the width prose is wrapped to on a wider terminal, so
	// lines stay readable.
	statsMaxWidth = 100
	// statsUnknownWidth is the width assumed for output that is not a
	// terminal (a pipe, a file): the full layout.
	statsUnknownWidth = 100
	// statsBaseWidth is the width the screen's right-aligned labels line up
	// to. A long sparkline widens it.
	statsBaseWidth = 72
	// statsMinWidth is the narrowest terminal the screen adapts to.
	statsMinWidth = 50

	minLabelWidth = 8
	// minBarWidth is the narrowest a bar is shrunk to before a row is cut
	// to fit.
	minBarWidth = 6

	statsAgentBar     = 22
	statsModelBar     = 14
	statsProjectBar   = 14
	statsCompositionW = 50
	statsNameLimit    = 24
	// statsFilterLimit cuts a --harness or --model value in the heading.
	statsFilterLimit   = 30
	statsMaxModelRows  = 6
	statsMaxGroupRows  = 60
	statsMaxProjectRow = 25
)

// statsGlyphs are the characters the screen draws with. Bars and sparklines
// use Unicode block characters; ASCII is the fallback for a locale that is
// not UTF-8 and for a dumb terminal.
type statsGlyphs struct {
	filled       string
	empty        string
	spark        [8]string
	sparkUnknown string
	segments     [4]string
	up           string
	down         string
	sep          string
	ellipsis     string
}

var unicodeGlyphs = statsGlyphs{
	filled: "█", empty: "░",
	spark:        [8]string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"},
	sparkUnknown: "·",
	segments:     [4]string{"█", "▓", "▒", "░"},
	up:           "▲", down: "▼", sep: "·", ellipsis: "…",
}

var asciiGlyphs = statsGlyphs{
	filled: "#", empty: ".",
	spark:        [8]string{"_", ".", ":", "-", "=", "+", "*", "#"},
	sparkUnknown: "?",
	segments:     [4]string{"#", "=", "-", "."},
	up:           "+", down: "-", sep: "-", ellipsis: "...",
}

// statsView is how a stats screen is drawn: the terminal's width and style,
// the characters to draw with, and the filters to name in the heading.
type statsView struct {
	style   textStyle
	width   int
	glyphs  statsGlyphs
	filters statsFilters
}

// statsWidthOutput is an output that says its own width; the stats goldens
// write to one.
type statsWidthOutput interface{ terminalWidth() int }

// newStatsView is the view for writing to out: its style and width (a width
// of 0, for output that is not a terminal, means the full layout), and ASCII
// when the locale is not UTF-8.
func newStatsView(out io.Writer, env interface{ lookupEnv(string) (string, bool) }) statsView {
	style := styleFor(underlyingWriter(out))
	if w, ok := underlyingWriter(out).(statsWidthOutput); ok {
		style.width = w.terminalWidth()
	}
	glyphs := unicodeGlyphs
	if !localeIsUTF8(env) {
		glyphs = asciiGlyphs
	}
	return statsView{style: style, width: style.width, glyphs: glyphs}
}

// localeIsUTF8 reads the locale as the C library does: the first of
// LC_ALL, LC_CTYPE and LANG that is set names the character set. Nothing
// set, or a name with UTF-8 in it, is Unicode; "C" and "POSIX" and any other
// name are not. A dumb terminal is not either.
func localeIsUTF8(env interface{ lookupEnv(string) (string, bool) }) bool {
	if term, _ := env.lookupEnv("TERM"); term == "dumb" {
		return false
	}
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if value, _ := env.lookupEnv(name); value != "" {
			lower := strings.ToLower(value)
			return strings.Contains(lower, "utf-8") || strings.Contains(lower, "utf8")
		}
	}
	return true
}

// wrap breaks a message to the view's width (at most statsMaxWidth).
func (v statsView) wrap(text string) string {
	width := v.width
	if width <= 0 {
		width = statsUnknownWidth
	}
	return hangingIndent("", text, min(max(width, statsMinWidth), statsMaxWidth))
}

// statsPrinter builds the screen line by line.
type statsPrinter struct {
	s      stats.Stats
	v      statsView
	g      statsGlyphs
	full   bool
	width  int  // the terminal's width
	cw     int  // the width right-aligned labels line up to
	approx bool // some cost shown is approximate (~)
	part   bool // some cost shown leaves out unpriced tokens (+)
	// asOfShown is whether the overview already says when the prices are
	// from, so the footer need not.
	asOfShown bool
}

// renderStats writes the stats screen for s to w. It draws the full layout,
// with bars, at statsFullWidth (80) columns or more, and a compact table
// below that; color only when the view's style has it. No line is wider than
// the terminal.
func renderStats(w io.Writer, s stats.Stats, v statsView) error {
	width := v.width
	if width <= 0 {
		width = statsUnknownWidth
	}
	width = max(width, statsMinWidth)
	p := &statsPrinter{
		s: s, v: v, g: v.glyphs, width: width, full: width >= statsFullWidth, cw: min(width, statsBaseWidth),
	}
	sections := [][]string{
		p.header(), p.tokensByDay(), p.overview(), p.agents(), p.costByModel(), p.topProjects(),
		p.composition(), p.highlights(), p.grouped(), p.footer(),
	}
	var out []string
	for _, section := range sections {
		if len(section) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, section...)
	}
	for _, line := range out {
		terminal.Println(w, line)
	}
	return nil
}

func (p *statsPrinter) bold(text string) string { return p.v.style.bold(text) }

func (p *statsPrinter) dim(text string) string { return p.v.style.dim(text) }

// heading is a section's title, with an optional right-aligned note.
func (p *statsPrinter) heading(title, note string) string {
	if note == "" {
		return p.bold(title)
	}
	gap := max(p.cw-visibleWidth(title)-visibleWidth(note), 2)
	return p.bold(title) + strings.Repeat(" ", gap) + p.dim(note)
}

// wrap breaks text to the screen's width.
func (p *statsPrinter) wrap(text string) []string {
	return strings.Split(hangingIndent("", text, min(p.width, statsMaxWidth)), "\n")
}

func (p *statsPrinter) header() []string {
	s := p.s
	window := fmt.Sprintf("last %d days", s.Window.Days)
	if s.Window.Days == 1 {
		window = "today"
	}
	parts := []string{"agent-archive stats", window}
	if f := p.filterText(); f != "" {
		parts = append(parts, f)
	}
	agents := "1 agent"
	if s.Coverage.Agents != 1 {
		agents = fmt.Sprintf("%d agents", s.Coverage.Agents)
	}
	sessions := commaInt(int64(s.Coverage.Sessions)) + " sessions"
	if s.Coverage.Sessions == 1 {
		sessions = "1 session"
	}
	if s.Coverage.SessionsWithTokens != s.Coverage.Sessions {
		sessions += fmt.Sprintf(" (%s with token data)", commaInt(int64(s.Coverage.SessionsWithTokens)))
	}
	parts = append(parts, agents, sessions)
	sep := " " + p.g.sep + " "
	var lines []string
	line := ""
	for _, part := range parts {
		switch {
		case line == "":
			line = part
		case visibleWidth(line)+visibleWidth(sep)+visibleWidth(part) > p.width:
			lines = append(lines, line)
			line = part
		default:
			line += sep + part
		}
	}
	lines = append(lines, line)
	lines[0] = p.bold(lines[0])
	return lines
}

// filterText names the filters the numbers were narrowed by.
func (p *statsPrinter) filterText() string {
	var parts []string
	f := p.v.filters
	if f.Harness != "" {
		parts = append(parts, "harness "+truncateVisible(archive.DisplayLine(f.Harness), statsFilterLimit))
	}
	if f.Model != "" {
		parts = append(parts, "model "+truncateVisible(archive.DisplayLine(f.Model), statsFilterLimit))
	}
	if f.Origin != "" {
		parts = append(parts, f.Origin+" only")
	}
	return strings.Join(parts, ", ")
}

func (p *statsPrinter) tokensByDay() []string {
	s := p.s
	if len(s.Daily) == 0 || s.Peak == nil && p.noTokenData() {
		return nil
	}
	note := ""
	if s.Peak != nil {
		note = "peak " + tokenCount(s.Peak.Tokens) + " " + p.g.sep + " " + p.dayLabel(s.Peak.Date)
	}
	line, span := p.sparkline()
	first, last := p.dayLabel(s.Daily[0].Date), p.dayLabel(s.Daily[len(s.Daily)-1].Date)
	gap := max(span-visibleWidth(first)-visibleWidth(last), 1)
	// The heading's note lines up with the widest of the screen and the
	// sparkline.
	saved := p.cw
	p.cw = max(p.cw, span)
	title := p.heading("TOKENS BY DAY", note)
	p.cw = saved
	labels := first + strings.Repeat(" ", gap) + last
	if len(s.Daily) == 1 {
		// One day has one date.
		labels = first
	}
	return []string{title, line, p.dim(labels)}
}

// noTokenData is whether no session in the window reports token counts.
func (p *statsPrinter) noTokenData() bool { return p.s.Coverage.SessionsWithTokens == 0 }

// sparkline draws the daily tokens, one character per day, or per run of days
// when the window has more days than the screen has columns. Its height
// scales to the busiest day. A day without sessions is the lowest bar, and a
// day whose sessions report no tokens is a mark of its own.
func (p *statsPrinter) sparkline() (string, int) {
	days := p.s.Daily
	perCell := 1
	for (len(days)+perCell-1)/perCell > p.width-2 {
		perCell++
	}
	type cell struct {
		perDay float64
		known  bool
	}
	var cells []cell
	var peak float64
	for i := 0; i < len(days); i += perCell {
		run := days[i:min(i+perCell, len(days))]
		var c cell
		for _, d := range run {
			if d.Tokens != nil {
				c.perDay += float64(*d.Tokens)
				c.known = true
			}
		}
		// A cell of several days is the average day, so a shorter last
		// cell is not drawn lower for having fewer days.
		c.perDay /= float64(len(run))
		peak = math.Max(peak, c.perDay)
		cells = append(cells, c)
	}
	var b strings.Builder
	for _, c := range cells {
		switch {
		case !c.known:
			b.WriteString(p.g.sparkUnknown)
		case peak == 0:
			b.WriteString(p.g.spark[0])
		default:
			b.WriteString(p.g.spark[int(math.Round(c.perDay/peak*float64(len(p.g.spark)-1)))])
		}
	}
	return b.String(), len(cells)
}

func (p *statsPrinter) overview() []string {
	o := p.s.Overview
	note := ""
	if p.anyDelta() {
		note = fmt.Sprintf("vs previous %d days", p.s.Window.Days)
		if p.s.Window.Days == 1 {
			note = "vs the day before"
		}
	}
	type row struct {
		label string
		value string
		delta string
		extra string
	}
	rows := []row{
		{"Sessions", commaMeasure(o.Sessions), p.delta(o.Sessions), ""},
		{"Prompts", commaMeasure(o.Prompts), p.delta(o.Prompts), ""},
		{"Tokens", tokenMeasure(o.Tokens), p.delta(o.Tokens), ""},
		{"Est. cost", p.costMeasureText(o), p.delta(o.Cost.Measure), ""},
		{"Active days", fmt.Sprintf("%d/%d", activeDays(o), o.DaysInWindow), "", p.streakText(o)},
	}
	if o.Tokens.Value == nil {
		rows[2].value = "unknown"
	}
	if o.Prompts.Value == nil {
		rows[1].value = "unknown"
	}
	if p.full {
		rows[3].extra = "at list price"
		if asOf := p.s.Prices.AsOf; asOf != "" {
			// The prices' date sits with the cost, so the screen says how
			// old the estimate can be where it is read.
			rows[3].extra += ", prices as of " + archive.DisplayLine(asOf)
			p.asOfShown = true
		}
	}
	labelW, valueW := 0, 0
	for _, r := range rows {
		labelW = max(labelW, visibleWidth(r.label))
		valueW = max(valueW, visibleWidth(r.value))
	}
	deltaW := 0
	for _, r := range rows {
		deltaW = max(deltaW, visibleWidth(r.delta))
	}
	lines := []string{p.heading("OVERVIEW", note)}
	for _, r := range rows {
		line := padRight(r.label, labelW) + "  " + padLeft(r.value, valueW)
		switch {
		case r.extra != "" && p.full:
			// The full layout lines the note up under the changes.
			line += "  " + padRight(r.delta, deltaW) + "  " + p.dim(r.extra)
		case r.extra != "":
			if r.delta != "" {
				line += "  " + r.delta
			}
			line += "  " + p.dim(r.extra)
		case r.delta != "":
			line += "  " + r.delta
		}
		lines = append(lines, strings.TrimRight(line, " "))
	}
	return lines
}

// anyDelta is whether the overview has any change against the previous
// period to show.
func (p *statsPrinter) anyDelta() bool {
	o := p.s.Overview
	for _, m := range []stats.Measure{o.Sessions, o.Prompts, o.Tokens, o.Cost.Measure} {
		if p.delta(m) != "" {
			return true
		}
	}
	return false
}

func activeDays(o stats.Overview) int {
	if o.ActiveDays.Value == nil {
		return 0
	}
	return int(math.Round(*o.ActiveDays.Value))
}

func (p *statsPrinter) streakText(o stats.Overview) string {
	switch {
	case o.CurrentStreak > 0:
		return fmt.Sprintf("streak %s (best %d)", plural(o.CurrentStreak, "day"), o.BestStreak)
	case o.BestStreak > 0:
		return fmt.Sprintf("no current streak (best %d)", o.BestStreak)
	}
	return ""
}

// delta is a measure's change against the previous period: an arrow and a
// percentage, "new" when the previous period had none, and nothing when
// either side is unknown.
func (p *statsPrinter) delta(m stats.Measure) string {
	if m.Value == nil || m.Previous == nil {
		return ""
	}
	if m.ChangePct == nil {
		if *m.Previous == 0 && *m.Value > 0 {
			return "new"
		}
		return ""
	}
	pct := math.Round(*m.ChangePct)
	switch {
	case pct > 0:
		return p.g.up + " " + commaInt(int64(pct)) + "%"
	case pct < 0:
		return p.g.down + " " + commaInt(int64(-pct)) + "%"
	}
	return "no change"
}

func commaMeasure(m stats.Measure) string {
	if m.Value == nil {
		return "unknown"
	}
	return commaInt(int64(math.Round(*m.Value)))
}

func tokenMeasure(m stats.Measure) string {
	if m.Value == nil {
		return "unknown"
	}
	return tokenCount(int64(math.Round(*m.Value)))
}

func (p *statsPrinter) costMeasureText(o stats.Overview) string {
	if o.Tokens.Value == nil {
		return "n/a"
	}
	return p.costText(o.Cost.Value, o.Cost.Approximate, o.Cost.Partial, false)
}

// costText is an estimated cost: n/a is what a missing amount reads as. A
// leading ~ marks an approximate cost and a trailing + one that leaves out
// unpriced tokens; the footer explains whichever appear.
func (p *statsPrinter) costText(usd *float64, approximate, partial, precise bool) string {
	if usd == nil {
		return "unpriced"
	}
	text := money(p.s.Prices.Currency, *usd, precise)
	if approximate {
		text = "~" + text
		p.approx = true
	}
	if partial {
		text += "+"
		p.part = true
	}
	return text
}

// tableCol is one right-aligned column of a table.
type tableCol struct {
	head  string
	cells []string
}

// tableBar is the bar drawn beside each row of a table: width cells at most,
// with shares (0 to 1) per row; a negative share leaves a row's bar blank.
type tableBar struct {
	width  int
	shares []float64
}

// table draws rows under a title: each row's label, an optional bar, and the
// columns, right-aligned under their heads on the title's line. A row that
// would run past the terminal is fitted to it: bars shrink first, then labels
// are cut, and last of all the bars go, so no line is wider than the terminal.
func (p *statsPrinter) table(title string, labels []string, bar *tableBar, cols []tableCol) []string {
	leftW := 0
	for _, l := range labels {
		leftW = max(leftW, visibleWidth(l))
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = visibleWidth(c.head)
		for _, cell := range c.cells {
			widths[i] = max(widths[i], visibleWidth(cell))
		}
	}
	colsW := 0
	for _, w := range widths {
		colsW += w + 2
	}
	barW := 0
	if bar != nil {
		barW = bar.width
	}
	// space is the columns a table takes with a bar barW wide and labels
	// labelW wide (the title may set the label column's width).
	space := func(barW, labelW int) int {
		barSpace := 0
		if barW > 0 {
			barSpace = barW + 2
		}
		return max(labelW, visibleWidth(title)-barSpace) + barSpace + colsW
	}
	for barW > minBarWidth && space(barW, leftW) > p.width {
		barW--
	}
	if barW > 0 && space(barW, min(leftW, minLabelWidth)) > p.width {
		barW = 0
	}
	barSpace := 0
	if barW > 0 {
		barSpace = barW + 2
	}
	leftW = max(leftW, visibleWidth(title)-barSpace)
	// A label that would push a row past the terminal is cut short.
	if excess := leftW + barSpace + colsW - p.width; excess > 0 && leftW > minLabelWidth {
		leftW = max(leftW-excess, minLabelWidth)
		labels = slices.Clone(labels)
		for i, l := range labels {
			labels[i] = truncateVisible(l, leftW)
		}
	}
	var head strings.Builder
	head.WriteString(p.bold(title) + strings.Repeat(" ", max(leftW+barSpace-visibleWidth(title), 0)))
	// Columns after the last one with a head have nothing to say over them:
	// the head line stops there, with no trailing blanks.
	lastHead := -1
	for i, c := range cols {
		if c.head != "" {
			lastHead = i
		}
	}
	for i, c := range cols[:lastHead+1] {
		head.WriteString("  " + p.dim(padLeft(c.head, widths[i])))
	}
	lines := []string{head.String()}
	for r, label := range labels {
		var line strings.Builder
		line.WriteString(padRight(label, leftW))
		if barW > 0 {
			line.WriteString("  " + p.bar(bar.shares[r], barW))
		}
		for i, c := range cols {
			line.WriteString("  " + padLeft(c.cells[r], widths[i]))
		}
		lines = append(lines, strings.TrimRight(line.String(), " "))
	}
	lines[0] = strings.TrimRight(lines[0], " ")
	return lines
}

// bar draws share (0 to 1) of width cells: filled up to it, empty after. A
// negative share is blank: nothing to draw.
func (p *statsPrinter) bar(share float64, width int) string {
	if share < 0 {
		return strings.Repeat(" ", width)
	}
	share = math.Min(1, share)
	n := int(math.Round(share * float64(width)))
	if share > 0 && n == 0 {
		n = 1
	}
	return strings.Repeat(p.g.filled, n) + strings.Repeat(p.g.empty, width-n)
}

func (p *statsPrinter) agents() []string {
	var labels, sessions, tokens, costs []string
	bar := &tableBar{width: statsAgentBar}
	for _, a := range p.s.Agents {
		labels = append(labels, archive.DisplayLine(a.Label))
		bar.shares = append(bar.shares, a.SessionShare)
		sessions = append(sessions, commaInt(int64(a.Sessions)))
		tokens = append(tokens, "unknown")
		costs = append(costs, "n/a")
		if a.Tokens != nil {
			tokens[len(tokens)-1] = tokenCount(*a.Tokens)
			costs[len(costs)-1] = p.costText(a.Cost.USD, a.Cost.Approximate, a.Cost.Partial, false)
		}
	}
	shares := make([]string, len(sessions))
	sessionW := 0
	for _, n := range sessions {
		sessionW = max(sessionW, len(n))
	}
	for i, a := range p.s.Agents {
		shares[i] = padLeft(sessions[i], sessionW) + " " + padLeft(percent(a.SessionShare), 4)
	}
	if !p.full {
		bar = nil
	}
	return p.table("AGENTS", labels, bar, []tableCol{
		{"sessions", shares}, {"tokens", tokens}, {"est. cost", costs},
	})
}

func (p *statsPrinter) costByModel() []string {
	rows := p.s.Models
	if len(rows) == 0 {
		return nil
	}
	more := 0
	if len(rows) > statsMaxModelRows {
		more = len(rows) - statsMaxModelRows
		rows = rows[:statsMaxModelRows]
	}
	var labels, costs, shares, notes []string
	bar := &tableBar{width: statsModelBar}
	for _, r := range rows {
		labels = append(labels, truncateVisible(archive.DisplayLine(r.Label), statsNameLimit))
		if !r.Priced || r.Cost.USD == nil {
			// An unpriced model has no share of the cost to draw; its
			// tokens are said instead.
			bar.shares = append(bar.shares, -1)
			costs = append(costs, "unpriced")
			shares = append(shares, tokenCount(r.Tokens)+" tokens")
			notes = append(notes, tokenCount(r.Tokens)+" tokens")
			continue
		}
		share := 0.0
		if r.CostShare != nil {
			share = *r.CostShare
		}
		bar.shares = append(bar.shares, share)
		costs = append(costs, p.costText(r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false))
		shares = append(shares, percent(share))
		notes = append(notes, "")
	}
	var lines []string
	if p.full {
		lines = p.table("COST BY MODEL", labels, bar, []tableCol{{"est. cost", costs}, {"", notes}})
	} else {
		lines = p.table("COST BY MODEL", labels, nil, []tableCol{{"est. cost", costs}, {"share", shares}})
	}
	if more > 0 {
		lines = append(lines, p.dim(fmt.Sprintf("+ %d more models (--json has them all)", more)))
	}
	return lines
}

func (p *statsPrinter) topProjects() []string {
	rows := p.s.Projects
	if len(rows) == 0 {
		return nil
	}
	var labels, sessions, tokens, costs []string
	bar := &tableBar{width: statsProjectBar}
	top := 0
	for _, r := range rows {
		top = max(top, r.Sessions)
	}
	for _, r := range rows {
		labels = append(labels, projectLabel(r.Name))
		share := 0.0
		if top > 0 {
			share = float64(r.Sessions) / float64(top)
		}
		bar.shares = append(bar.shares, share)
		sessions = append(sessions, commaInt(int64(r.Sessions)))
		tokens = append(tokens, "unknown")
		costs = append(costs, "n/a")
		if r.Tokens != nil {
			tokens[len(tokens)-1] = tokenCount(*r.Tokens)
			costs[len(costs)-1] = p.costText(r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
		}
	}
	if !p.full {
		bar = nil
	}
	lines := p.table("TOP PROJECTS", labels, bar, []tableCol{{"sessions", sessions}, {"tokens", tokens}, {"est. cost", costs}})
	if more := p.s.TotalProjects - len(rows); more > 0 {
		lines = append(lines, p.dim(fmt.Sprintf("+ %d more (--by project lists them)", more)))
	}
	return lines
}

// projectLabel is a project's name as shown: sanitized and cut short, with a
// stand-in for sessions that carry none.
func projectLabel(name string) string {
	if name == "" {
		return "(no project)"
	}
	return truncateVisible(archive.DisplayLine(name), statsNameLimit)
}

func (p *statsPrinter) composition() []string {
	c := p.s.Composition
	if c == nil || c.Total == 0 {
		return nil
	}
	type segment struct {
		label string
		stats.Segment
	}
	segments := []segment{
		{"Cache read", c.CacheRead}, {"Cache write", c.CacheWrite},
		{"Input", c.FreshInput}, {"Output", c.Output},
	}
	shares := make([]float64, len(segments))
	for i, seg := range segments {
		shares[i] = seg.Share
	}
	lines := []string{p.bold("WHAT USED YOUR TOKENS")}
	var legend []string
	if p.full {
		var bar strings.Builder
		for i, n := range apportion(shares, statsCompositionW) {
			bar.WriteString(strings.Repeat(p.g.segments[i], n))
		}
		lines = append(lines, bar.String())
	}
	labelW := 0
	for _, seg := range segments {
		labelW = max(labelW, len(seg.label))
	}
	var inline []string
	for i, seg := range segments {
		inline = append(inline, fmt.Sprintf("%s %s %s %s", p.g.segments[i], seg.label, percent(seg.Share), tokenCount(seg.Tokens)))
		legend = append(legend, fmt.Sprintf("%s %s  %4s  %s", p.g.segments[i], padRight(seg.label, labelW), percent(seg.Share), tokenCount(seg.Tokens)))
	}
	// The legend goes on one line under the bar when it fits, with three
	// spaces between the segments or, when that is too wide for the
	// terminal, two.
	joined := strings.Join(inline, "   ")
	if visibleWidth(joined) > p.width {
		joined = strings.Join(inline, "  ")
	}
	if p.full && visibleWidth(joined) <= p.width {
		lines = append(lines, joined)
	} else {
		lines = append(lines, legend...)
	}
	if c.ReasoningOfOutput != nil && *c.ReasoningOfOutput > 0 {
		lines = append(lines, p.dim("Output includes "+tokenCount(*c.ReasoningOfOutput)+" reasoning tokens."))
	}
	labelWidth := len("Subagents")
	if sub := p.s.Subagents; sub != nil {
		lines = append(lines, padRight("Subagents", labelWidth)+"  "+
			fmt.Sprintf("%s of tokens (%s) in %s", percent(sub.Share), tokenCount(sub.Tokens), plural(sub.Sessions, "session")))
	}
	if len(p.s.Skills) > 0 {
		var names []string
		for _, sk := range p.s.Skills {
			names = append(names, fmt.Sprintf("%s %d", truncateVisible(archive.DisplayLine(sk.Name), statsNameLimit), sk.Sessions))
		}
		lines = append(lines, p.hang(padRight("Skills", labelWidth)+"  ", strings.Join(names, " "+p.g.sep+" ")))
	}
	if m := p.s.MCP; m != nil && len(m.Servers) > 0 {
		var names []string
		for _, srv := range m.Servers {
			names = append(names, fmt.Sprintf("%s %s", truncateVisible(archive.DisplayLine(srv.Name), statsNameLimit), commaInt(srv.Calls)))
		}
		lines = append(lines, p.hang(padRight("MCP", labelWidth)+"  ", strings.Join(names, " "+p.g.sep+" ")))
	}
	var notes []string
	switch {
	case len(p.s.Skills) > 0 && p.s.MCP != nil:
		notes = append(notes, "Skills count sessions that used each one; MCP counts calls.")
	case len(p.s.Skills) > 0:
		notes = append(notes, "Skills count sessions that used each one.")
	case p.s.MCP != nil:
		notes = append(notes, "MCP counts calls.")
	}
	if p.s.MCP != nil {
		notes = append(notes, "MCP: "+archive.DisplayLine(p.s.MCP.Scope))
	}
	for _, note := range notes {
		for _, line := range p.wrap(note) {
			lines = append(lines, p.dim(line))
		}
	}
	return lines
}

// hang prints text after prefix, wrapping to the screen's width with
// following lines indented under it. A colored prefix is not passed: prefix
// is plain.
func (p *statsPrinter) hang(prefix, text string) string {
	return hangingIndent(prefix, text, min(p.width, statsMaxWidth))
}

func (p *statsPrinter) highlights() []string {
	h := p.s.Highlights
	labelW := len("Busiest day")
	type item struct {
		label string
		text  string
	}
	var items []item
	if h.BusiestDay != nil {
		items = append(items, item{"Busiest day", fmt.Sprintf("%s (%s)", p.dayLabel(h.BusiestDay.Date), plural(h.BusiestDay.Sessions, "session"))})
	}
	favorite := ""
	if h.FavoriteModel != nil {
		favorite = truncateVisible(archive.DisplayLine(h.FavoriteModel.Label), statsNameLimit)
		if h.FavoriteModel.By == "tokens" {
			favorite += " (by tokens)"
		}
	}
	lines := []string{p.bold("HIGHLIGHTS")}
	// The busiest day and the favorite model share a line when the terminal
	// has room; otherwise each has its own, and the labels line up under
	// the longest.
	pair := ""
	if favorite != "" && len(items) > 0 && p.full {
		left := padRight(items[0].label, labelW) + "  " + items[0].text
		pair = padRight(left, 38) + "Favorite model  " + favorite
	}
	switch {
	case pair != "" && visibleWidth(pair) <= p.width:
		lines = append(lines, pair)
		items = nil
	case favorite != "":
		items = append(items, item{"Favorite model", favorite})
		labelW = len("Favorite model")
	}
	for _, it := range items {
		lines = append(lines, padRight(it.label, labelW)+"  "+it.text)
	}
	if c := h.CostliestSession; c != nil && c.Cost.USD != nil {
		text := p.costText(c.Cost.USD, c.Cost.Approximate, c.Cost.Partial, true)
		if c.Project != "" {
			text += "  " + projectLabel(c.Project)
		}
		if drivers := p.drivers(c); drivers != "" {
			text += "  (" + drivers + ")"
		}
		lines = append(lines, p.hang(padRight("Costliest", labelW)+"  ", text))
	}
	if t := h.ToolErrors; t != nil {
		text := fmt.Sprintf("%s of %s tool results flagged as errors, %s measured", ratePercent(t.Rate), commaInt(t.Results), plural(t.Sessions, "session"))
		if t.UnknownSessions > 0 {
			text += fmt.Sprintf(" (%d do not record them)", t.UnknownSessions)
		}
		lines = append(lines, p.hang(padRight("Tool errors", labelW)+"  ", text))
	}
	if m := h.MonthRank; m != nil && m.Of > 1 {
		lines = append(lines, p.hang("", "This month so far is your "+ordinalHeaviest(m.Rank)+p.monthsOf(m)))
	}
	if len(lines) == 1 {
		return nil
	}
	return lines
}

func (p *statsPrinter) monthsOf(m *stats.MonthRank) string {
	if m.Of >= stats.MonthsCompared {
		return fmt.Sprintf(" of the last %d months", m.Of)
	}
	return fmt.Sprintf(" of the %d months with token data", m.Of)
}

// ordinalHeaviest is a month's rank by tokens in words: "heaviest",
// "2nd-heaviest".
func ordinalHeaviest(rank int) string {
	if rank <= 1 {
		return "heaviest"
	}
	return ordinal(rank) + "-heaviest"
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// drivers says what likely made the costliest session costly.
func (p *statsPrinter) drivers(c *stats.CostliestSession) string {
	var parts []string
	if slices.Contains(c.Drivers, stats.DriverLongContext) {
		parts = append(parts, "long context")
	}
	if slices.Contains(c.Drivers, stats.DriverSubagents) {
		parts = append(parts, plural(c.Subagents, "subagent"))
	}
	if slices.Contains(c.Drivers, stats.DriverLowCacheHit) && c.CacheHitRate != nil {
		parts = append(parts, percent(*c.CacheHitRate)+" cache hit")
	}
	return strings.Join(parts, ", ")
}

// grouped is the --by breakdown.
func (p *statsPrinter) grouped() []string {
	g := p.s.Groups
	if g == nil || len(g.Rows) == 0 {
		return nil
	}
	title := "BY " + strings.ToUpper(string(g.By))
	rows := g.Rows
	limit := statsMaxGroupRows
	if g.By == stats.GroupProject {
		limit = statsMaxProjectRow
	}
	more := 0
	if len(rows) > limit {
		more = len(rows) - limit
		if g.By == stats.GroupProject {
			rows = rows[:limit]
		} else {
			// Chronological rows: keep the newest.
			rows = rows[more:]
		}
	}
	var labels, sessions, prompts, tokens, costs []string
	for _, r := range rows {
		if g.By == stats.GroupProject {
			labels = append(labels, projectLabel(r.Key))
		} else {
			labels = append(labels, r.Key)
		}
		sessions = append(sessions, commaInt(int64(r.Sessions)))
		prompts, tokens = append(prompts, "unknown"), append(tokens, "unknown")
		if r.Prompts != nil {
			prompts[len(prompts)-1] = commaInt(*r.Prompts)
		}
		costs = append(costs, "n/a")
		if r.Tokens != nil {
			tokens[len(tokens)-1] = tokenCount(*r.Tokens)
			costs[len(costs)-1] = p.costText(r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
		}
	}
	lines := p.table(title, labels, nil, []tableCol{{"sessions", sessions}, {"prompts", prompts}, {"tokens", tokens}, {"est. cost", costs}})
	if g.By == stats.GroupWeek {
		lines = append(lines, p.dim("Weeks start on Monday."))
	}
	switch {
	case more > 0 && g.By == stats.GroupProject:
		lines = append(lines, p.dim(fmt.Sprintf("+ %d more (--json has them all)", more)))
	case more > 0:
		lines = append(lines, p.dim(fmt.Sprintf("%d earlier rows not shown (--json has them all)", more)))
	}
	return lines
}

func (p *statsPrinter) footer() []string {
	s := p.s
	var lines []string
	scope := "Scope: this archive only."
	if unknown := p.unknownTokenText(); unknown != "" {
		scope += " " + unknown
	}
	lines = append(lines, p.wrap(scope)...)
	cost := "Cost is an estimate at list price, not a bill."
	if s.Prices.Version != "" {
		cost += " Prices " + archive.DisplayLine(s.Prices.Version)
		if !p.asOfShown {
			cost += ", as of " + archive.DisplayLine(s.Prices.AsOf)
		}
		if s.Prices.Overridden {
			cost += ", with your --prices file applied"
		}
		cost += "."
	}
	lines = append(lines, p.wrap(cost)...)
	if p.approx {
		lines = append(lines, p.wrap(fmt.Sprintf("~ priced at the session's main model for %s with no per-model split (metadata from before parser 0.14.0).",
			plural(s.Coverage.SessionsPricedAtMainModel, "session")))...)
	}
	if p.part {
		lines = append(lines, p.wrap("+ leaves out tokens of models the price table does not list"+p.unpricedModels()+".")...)
	}
	if s.Groups == nil {
		lines = append(lines, p.wrap("--by day|week|month|project "+p.g.sep+" --json")...)
	}
	for i := range lines {
		lines[i] = p.dim(lines[i])
	}
	return lines
}

// unknownTokenText names the agents whose sessions report no tokens, which
// the totals leave out.
func (p *statsPrinter) unknownTokenText() string {
	var parts []string
	for _, a := range p.s.Agents {
		if a.UnknownTokenSessions > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", archive.DisplayLine(a.Label), a.UnknownTokenSessions))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Sessions with no token data are left out of token and cost totals (" + strings.Join(parts, ", ") + ")."
}

// unpricedModels lists, in parentheses, the models the window used that the
// price table does not list.
func (p *statsPrinter) unpricedModels() string {
	var names []string
	for _, r := range p.s.Models {
		if !r.Priced || r.Cost.USD == nil {
			names = append(names, truncateVisible(archive.DisplayLine(r.Label), statsNameLimit))
		}
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) > 3 {
		names = append(names[:3], p.g.ellipsis)
	}
	return " (" + strings.Join(names, ", ") + ")"
}

// dayLabel is a "2006-01-02" date as "Sep 17", with the year when the window
// is long enough for it to be ambiguous.
func (p *statsPrinter) dayLabel(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return archive.DisplayLine(date)
	}
	if p.s.Window.Days > 300 {
		return t.Format("Jan 2 2006")
	}
	return t.Format("Jan 2")
}

// apportion splits total cells among shares (each 0 to 1, summing to about
// 1), by the largest remainder so the cells add up exactly, and gives every
// nonzero share at least one cell.
func apportion(shares []float64, total int) []int {
	cells := make([]int, len(shares))
	type remainder struct {
		index int
		frac  float64
	}
	var rest []remainder
	used := 0
	for i, s := range shares {
		exact := s * float64(total)
		cells[i] = int(exact)
		if s > 0 && cells[i] == 0 {
			cells[i] = 1
		}
		used += cells[i]
		rest = append(rest, remainder{i, exact - math.Floor(exact)})
	}
	for used < total && len(rest) > 0 {
		best := 0
		for i, r := range rest {
			if r.frac > rest[best].frac {
				best = i
			}
		}
		if shares[rest[best].index] > 0 {
			cells[rest[best].index]++
			used++
		}
		rest[best].frac = -1
		allDone := true
		for _, r := range rest {
			if r.frac >= 0 {
				allDone = false
			}
		}
		if allDone {
			break
		}
	}
	// A minimum of one cell can overshoot the total; take it back from the
	// widest segment.
	for used > total {
		widest := 0
		for i, n := range cells {
			if n > cells[widest] {
				widest = i
			}
		}
		cells[widest]--
		used--
	}
	return cells
}

func padRight(text string, width int) string {
	if gap := width - visibleWidth(text); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

func padLeft(text string, width int) string {
	if gap := width - visibleWidth(text); gap > 0 {
		return strings.Repeat(" ", gap) + text
	}
	return text
}

// commaInt is n with thousands separators: 3,204.
func commaInt(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// maxShownTokens and maxShownMoney are where a number is shown as a bound: a
// saturated sum, not a measurement.
const (
	maxShownTokens = 1e15
	maxShownMoney  = 1e12
)

// tokenCount is a token total in its shortest form: 812, 4.9K, 61M, 1.2B.
// Under ten of a unit it keeps one decimal, beyond that none.
func tokenCount(n int64) string {
	if n >= maxShownTokens {
		// Sums saturate at the largest int64; no real usage is near this.
		return ">999T"
	}
	units := []struct {
		size   float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}}
	value := float64(n)
	for i, u := range units {
		if value >= u.size {
			v := value / u.size
			if v < 9.95 {
				return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + u.suffix
			}
			rounded := math.Round(v)
			if rounded >= 1000 {
				// 999.6K reads as 1M, not 1000K.
				if i == 0 {
					return ">999T"
				}
				return "1" + units[i-1].suffix
			}
			return strconv.FormatFloat(rounded, 'f', 0, 64) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// money is an amount of currency. USD is "$" and any other currency its code
// and a space. An amount from ten up has no cents unless precise.
func money(currency string, amount float64, precise bool) string {
	symbol := "$"
	if currency != "" && currency != "USD" {
		symbol = archive.DisplayLine(currency) + " "
	}
	if amount >= maxShownMoney {
		return symbol + ">999B"
	}
	if !precise && amount >= 10 {
		return symbol + commaInt(int64(math.Round(amount)))
	}
	whole, cents, _ := strings.Cut(strconv.FormatFloat(amount, 'f', 2, 64), ".")
	n, _ := strconv.ParseInt(whole, 10, 64)
	return symbol + commaInt(n) + "." + cents
}

// percent is a share (0 to 1) as a whole percentage, "<1%" for a nonzero
// share under half a percent.
func percent(share float64) string {
	if share > 0 && share < 0.005 {
		return "<1%"
	}
	return strconv.FormatFloat(math.Round(share*100), 'f', 0, 64) + "%"
}

// ratePercent is a rate (0 to 1) with one decimal under ten percent.
func ratePercent(rate float64) string {
	if rate > 0 && rate < 0.1 {
		return strconv.FormatFloat(rate*100, 'f', 1, 64) + "%"
	}
	return percent(rate)
}
