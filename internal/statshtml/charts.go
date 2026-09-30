package statshtml

import (
	"fmt"
	"math"
	"strconv"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// Geometry of the daily chart, in pixels. The chart's width is the page's, so
// x is always a percentage and y always pixels: text never scales.
const (
	chartHeight   = 196
	chartPlotTop  = 30
	chartBaseline = 168
	chartAxisY    = 188
	// slotPx is the width one bar's slot takes at the chart's widest; a chart
	// with few bars is capped at that many slots so its bars stay slim.
	slotPx      = 34
	minChartPx  = 280
	barFraction = 0.72
)

// bucket is one bar of the daily chart: a day, or a run of days when the
// window has more days than bars.
type bucket struct {
	first    string
	last     string
	sessions int
	// tokens is the busiest day's tokens of the run, and known whether any day
	// of it reports tokens.
	tokens  int64
	known   bool
	hasPeak bool
}

// daily is the tokens-by-day chart, or a sentence saying why there is none.
func (b *builder) daily() (*dailyChart, string) {
	s := b.s
	if len(s.Daily) == 0 {
		return nil, ""
	}
	if s.Coverage.SessionsWithTokens == 0 {
		return nil, "No session in this window reports token counts, so there is no token chart."
	}
	per := (len(s.Daily) + maxBars - 1) / maxBars
	var buckets []bucket
	for i := 0; i < len(s.Daily); i += per {
		run := s.Daily[i:min(i+per, len(s.Daily))]
		bk := bucket{first: run[0].Date, last: run[len(run)-1].Date}
		for _, d := range run {
			bk.sessions += d.Sessions
			if d.Tokens != nil {
				bk.known = true
				bk.tokens = max(bk.tokens, *d.Tokens)
			}
			if s.Peak != nil && d.Date == s.Peak.Date {
				bk.hasPeak = true
			}
		}
		buckets = append(buckets, bk)
	}
	var top int64
	for _, bk := range buckets {
		top = max(top, bk.tokens)
	}
	n := len(buckets)
	slot := 100 / float64(n)
	c := &dailyChart{
		Height: chartHeight, Baseline: chartBaseline, PlotTop: chartPlotTop, AxisY: chartAxisY,
		MaxWidth: max(n*slotPx, minChartPx),
	}
	plotH := float64(chartBaseline - chartPlotTop)
	for i, bk := range buckets {
		barTop := float64(chartBaseline)
		bar := dayBar{
			SlotX: num(float64(i) * slot), SlotW: num(slot),
			X: num(float64(i)*slot + slot*(1-barFraction)/2), W: num(slot * barFraction),
			Title: b.bucketTitle(bk, per),
		}
		switch {
		case !bk.known && bk.sessions > 0:
			// Sessions that report no tokens: a mark of its own, not a zero.
			bar.Class, bar.Y, bar.H = "unknown", num(chartBaseline-5), "5"
		case bk.tokens == 0:
			bar.Class, bar.Y, bar.H = "zero", num(chartBaseline-1), "1"
		default:
			h := math.Max(plotH*float64(bk.tokens)/float64(top), 2)
			barTop = chartBaseline - h
			bar.Class, bar.Y, bar.H = "day", num(barTop), num(h)
			if bk.hasPeak {
				bar.Class = "day peak"
			}
		}
		c.Bars = append(c.Bars, bar)
		if bk.hasPeak && s.Peak != nil {
			c.PeakLabel = peakLabel(i, slot, barTop, "Peak "+tokenCount(s.Peak.Tokens)+" · "+dayLabel(s.Peak.Date, s.Window.Days))
			c.Summary = fmt.Sprintf("Tokens by day, %s to %s. Peak %s tokens on %s.", dayLabel(s.Window.FirstDay, s.Window.Days),
				dayLabel(s.Window.LastDay, s.Window.Days), tokenCount(s.Peak.Tokens), dayLabel(s.Peak.Date, s.Window.Days))
		}
		c.Rows = append(c.Rows, dayRow{Label: bucketLabel(bk, s.Window.Days), Sessions: commaInt(int64(bk.sessions)), Tokens: bucketTokens(bk)})
	}
	if c.Summary == "" {
		c.Summary = fmt.Sprintf("Tokens by day, %s to %s.", dayLabel(s.Window.FirstDay, s.Window.Days), dayLabel(s.Window.LastDay, s.Window.Days))
	}
	c.XLabels = xLabels(buckets, slot, s.Window.Days)
	c.Caption = "Each bar is one day."
	if per > 1 {
		c.Caption = fmt.Sprintf("Each bar is %d days and shows its busiest day.", per)
	}
	c.Caption += " A short mark means sessions that record no tokens."
	return c, ""
}

func bucketLabel(bk bucket, windowDays int) string {
	if bk.first == bk.last {
		return dayLabel(bk.first, windowDays)
	}
	return dayLabel(bk.first, windowDays) + " to " + dayLabel(bk.last, windowDays)
}

func bucketTokens(bk bucket) string {
	if !bk.known {
		if bk.sessions == 0 {
			return "0"
		}
		return "unknown"
	}
	return tokenCount(bk.tokens)
}

// bucketTitle is the tooltip of a bar: the day (or run of days), its tokens and
// its sessions.
func (b *builder) bucketTitle(bk bucket, per int) string {
	label := bucketLabel(bk, b.s.Window.Days)
	switch {
	case bk.sessions == 0:
		return label + " · no sessions"
	case !bk.known:
		return label + " · " + plural(bk.sessions, "session") + " · token count unknown"
	case per > 1:
		return label + " · busiest day " + tokenCount(bk.tokens) + " tokens · " + plural(bk.sessions, "session")
	}
	return label + " · " + tokenCount(bk.tokens) + " tokens · " + plural(bk.sessions, "session")
}

// peakLabel places the peak's direct label above its bar, kept inside the
// chart by anchoring it to the bar's near edge when the bar is close to a side.
func peakLabel(i int, slot, barTop float64, text string) *chartLabel {
	center := (float64(i) + 0.5) * slot
	l := &chartLabel{X: num(center), Anchor: "middle", Text: text}
	switch {
	case center < 18:
		l.X, l.Anchor = num(float64(i)*slot), "start"
	case center > 82:
		l.X, l.Anchor = num(float64(i+1)*slot), "end"
	}
	l.Y = num(math.Max(barTop-8, 14))
	return l
}

// xLabels are the dates under the chart: the first and last, and a few
// between them when they fit (fewer when the labels carry a year).
func xLabels(buckets []bucket, slot float64, windowDays int) []chartLabel {
	n := len(buckets)
	labels := []chartLabel{{X: "0", Anchor: "start", Text: dayLabel(buckets[0].first, windowDays)}}
	between := 2
	if windowDays > 300 {
		between = 1
	}
	if n >= 12 {
		for j := 1; j <= between; j++ {
			i := int(math.Round(float64(n) * float64(j) / float64(between+1)))
			if i <= 0 || i >= n-1 {
				continue
			}
			labels = append(labels, chartLabel{X: num((float64(i) + 0.5) * slot), Anchor: "middle", Text: dayLabel(buckets[i].first, windowDays)})
		}
	}
	if n > 1 {
		labels = append(labels, chartLabel{X: "100", Anchor: "end", Text: dayLabel(buckets[n-1].last, windowDays)})
	}
	for i := range labels {
		labels[i].Y = strconv.Itoa(chartAxisY)
		if labels[i].X != "0" {
			labels[i].X += "%"
		}
	}
	return labels
}

// Donut geometry, in the SVG's own units.
const (
	donutCenter = 100.0
	donutRadius = 68.0
	// donutGap is the surface-colored gap between segments, in units of arc.
	donutGap = 3.0
	// donutLabelShare is the smallest share a segment's direct label fits in.
	donutLabelShare = 0.08
)

// arc is where a donut segment is drawn and its direct label.
type arc struct {
	dash    string
	offset  string
	text    string
	textX   string
	textY   string
	hasText bool
}

// arcGeometry draws a segment of share (0 to 1) that starts start units along
// a ring of circumference circ: a dash of the ring's stroke, shortened by the
// gap that separates it from its neighbors (a whole ring has none), and a
// label at the middle of the arc when the arc is wide enough to hold one. A
// segment with no share is not drawn.
func arcGeometry(share, start, circ float64) arc {
	length := share * circ
	if length <= 0 {
		return arc{}
	}
	visible := length
	if share < 0.999 {
		visible = math.Max(length-donutGap, 1)
	}
	dash, offset := num(visible)+" "+num(circ-visible), num(-(start + (length-visible)/2))
	if share < donutLabelShare {
		return arc{dash: dash, offset: offset}
	}
	angle := (start+length/2)/circ*2*math.Pi - math.Pi/2
	return arc{
		dash: dash, offset: offset, hasText: true, text: percent(share),
		textX: num(donutCenter + donutRadius*math.Cos(angle)), textY: num(donutCenter + donutRadius*math.Sin(angle)),
	}
}

// tokens is the composition donut with its legend and the rows that appear
// only when there is something to say.
func (b *builder) tokens() *tokenSection {
	c := b.s.Composition
	if c == nil || c.Total == 0 {
		return nil
	}
	t := &tokenSection{Total: tokenCount(c.Total)}
	parts := []struct {
		label string
		seg   stats.Segment
	}{{"Cache read", c.CacheRead}, {"Cache write", c.CacheWrite}, {"Fresh input", c.FreshInput}, {"Output", c.Output}}
	circ := 2 * math.Pi * donutRadius
	start := 0.0
	for i, part := range parts {
		length := part.seg.Share * circ
		g := arcGeometry(part.seg.Share, start, circ)
		t.Segments = append(t.Segments, segment{
			Class: "seg" + strconv.Itoa(i+1), Label: part.label, Tokens: tokenCount(part.seg.Tokens), Share: percent(part.seg.Share),
			Speak: fmt.Sprintf("%s: %s, %s of tokens", part.label, tokenCount(part.seg.Tokens), percent(part.seg.Share)),
			Dash:  g.dash, Offset: g.offset, HasText: g.hasText, Text: g.text, TextX: g.textX, TextY: g.textY,
		})
		start += length
	}
	if c.ReasoningOfOutput != nil && *c.ReasoningOfOutput > 0 {
		t.Reasoning = "Output includes " + tokenCount(*c.ReasoningOfOutput) + " reasoning tokens."
	}
	if sub := b.s.Subagents; sub != nil {
		t.Subagents = &subagentRow{
			Text: fmt.Sprintf("%s of tokens (%s) in %s", percent(sub.Share), tokenCount(sub.Tokens), plural(sub.Sessions, "session")),
			Pct:  pct(sub.Share),
		}
	}
	for _, sk := range b.s.Skills {
		t.Skills = append(t.Skills, nameCount{Name: clean(sk.Name), Count: plural(sk.Sessions, "session")})
	}
	if m := b.s.MCP; m != nil && len(m.Servers) > 0 {
		for _, srv := range m.Servers {
			t.MCP = append(t.MCP, nameCount{Name: clean(srv.Name), Count: callCount(srv.Calls)})
		}
	}
	switch {
	case len(b.s.Skills) > 0 && b.s.MCP != nil:
		t.Notes = append(t.Notes, "Skills count sessions that used each one; MCP counts calls.")
	case len(b.s.Skills) > 0:
		t.Notes = append(t.Notes, "Skills count sessions that used each one.")
	case b.s.MCP != nil:
		t.Notes = append(t.Notes, "MCP counts calls.")
	}
	if b.s.MCP != nil {
		t.Notes = append(t.Notes, "MCP: "+plain(b.s.MCP.Scope))
	}
	return t
}

// callCount is "1 call" or "12 calls".
func callCount(n int64) string {
	if n == 1 {
		return "1 call"
	}
	return commaInt(n) + " calls"
}
