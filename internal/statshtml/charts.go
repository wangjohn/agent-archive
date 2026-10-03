package statshtml

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
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
	// spend is the busiest day's estimated cost of the run and cost that day's
	// cost with its qualifiers; known is whether any day of the run has a cost.
	spend float64
	cost  stats.Cost
	// tokens is the busiest day's tokens of the run, and tokensKnown whether
	// any day of it reports tokens.
	tokens      int64
	tokensKnown bool
	hasPeak     bool
}

// spendOf is a day's estimated cost when it has one a chart can draw: a number
// that is not negative. The engine never returns another, but the page draws
// whatever it is given.
func spendOf(c stats.Cost) (float64, bool) {
	if c.USD == nil || !finite(*c.USD) || *c.USD < 0 {
		return 0, false
	}
	return *c.USD, true
}

// daily is the spend-by-day chart, or a sentence saying why there is none.
func (b *builder) daily() (*dailyChart, string) {
	s := b.s
	days := s.ChartDays()
	if len(days) == 0 {
		return nil, ""
	}
	if s.Coverage.SessionsWithTokens == 0 {
		return nil, "No session in this window reports token counts, so there is no spend to chart."
	}
	if s.PeakSpend == nil || !finite(s.PeakSpend.USD) || s.PeakSpend.USD <= 0 {
		return nil, "No day in this window has an estimated cost above zero, so there is no spend chart."
	}
	per := (len(days) + maxBars - 1) / maxBars
	var buckets []bucket
	for i := 0; i < len(days); i += per {
		run := days[i:min(i+per, len(days))]
		bk := bucket{first: run[0].Date, last: run[len(run)-1].Date}
		best := -1.0
		for _, d := range run {
			bk.sessions += d.Sessions
			if spend, ok := spendOf(d.Cost); ok && spend > best {
				best, bk.spend, bk.cost = spend, spend, d.Cost
			}
			if d.Tokens != nil {
				bk.tokensKnown = true
				bk.tokens = max(bk.tokens, *d.Tokens)
			}
			if d.Date == s.PeakSpend.Date {
				bk.hasPeak = true
			}
		}
		buckets = append(buckets, bk)
	}
	var top float64
	for _, bk := range buckets {
		top = math.Max(top, bk.spend)
	}
	n := len(buckets)
	slot := 100 / float64(n)
	c := &dailyChart{
		Height: chartHeight, Baseline: chartBaseline, PlotTop: chartPlotTop, AxisY: chartAxisY,
		MaxWidth: max(n*slotPx, minChartPx),
	}
	peak := b.peakText()
	plotH := float64(chartBaseline - chartPlotTop)
	for i, bk := range buckets {
		barTop := float64(chartBaseline)
		bar := dayBar{
			SlotX: num(float64(i) * slot), SlotW: num(slot),
			X: num(float64(i)*slot + slot*(1-barFraction)/2), W: num(slot * barFraction),
			Title: b.bucketTitle(bk, per),
		}
		switch {
		case bk.sessions > 0 && !bk.known():
			// Sessions with no cost: a mark of its own, not a zero.
			bar.Class, bar.Y, bar.H = "unknown", num(chartBaseline-5), "5"
		case bk.spend <= 0 || top <= 0:
			bar.Class, bar.Y, bar.H = "zero", num(chartBaseline-1), "1"
		default:
			h := math.Max(plotH*bk.spend/top, 2)
			barTop = chartBaseline - h
			bar.Class, bar.Y, bar.H = "day", num(barTop), num(h)
			if bk.hasPeak {
				bar.Class = "day peak"
			}
		}
		c.Bars = append(c.Bars, bar)
		if bk.hasPeak {
			c.PeakLabel = peakLabel(i, slot, barTop, "Peak "+peak+" · "+dayLabel(s.PeakSpend.Date, s.Window.Days))
		}
		c.Rows = append(c.Rows, b.dayRow(bk))
	}
	c.Summary = fmt.Sprintf("Spend by day, %s to %s. Peak %s on %s.", dayLabel(days[0].Date, s.Window.Days),
		dayLabel(s.Window.LastDay, s.Window.Days), peak, dayLabel(s.PeakSpend.Date, s.Window.Days))
	c.XLabels = xLabels(buckets, slot, s.Window.Days)
	c.Caption = "Each bar is one day."
	c.SpendHead, c.TokensHead = "Spend", "Tokens"
	if per > 1 {
		c.Caption = fmt.Sprintf("Each bar is %d days and shows its busiest day.", per)
		c.SpendHead, c.TokensHead = "Busiest day's spend", "Busiest day's tokens"
	}
	c.Caption += " Estimated at list price. A short grey mark means sessions that could not be priced."
	return c, ""
}

// known is whether the run has a day with an estimated cost.
func (bk bucket) known() bool { return bk.cost.USD != nil }

// peakText is the peak day's estimated cost as shown, with the qualifiers of
// that day's cost (a ~ for an approximate one, a + for a partial one).
func (b *builder) peakText() string {
	s := b.s
	var approximate, partial bool
	for _, d := range s.Daily {
		if d.Date == s.PeakSpend.Date {
			approximate, partial = d.Cost.Approximate, d.Cost.Partial
			break
		}
	}
	return b.cost.costText(s.Prices.Currency, &s.PeakSpend.USD, approximate, partial, false)
}

func bucketLabel(bk bucket, windowDays int) string {
	if bk.first == bk.last {
		return dayLabel(bk.first, windowDays)
	}
	return dayLabel(bk.first, windowDays) + " to " + dayLabel(bk.last, windowDays)
}

// spendText is a run's spend as shown: a plain 0 when it has no sessions (as its
// sessions and tokens are), unknown, never zero, when it has sessions but no
// cost.
func (b *builder) spendText(bk bucket) string {
	switch {
	case bk.sessions == 0:
		return "0"
	case bk.known():
		return b.cost.costText(b.s.Prices.Currency, bk.cost.USD, bk.cost.Approximate, bk.cost.Partial, false)
	}
	return "n/a"
}

func bucketTokens(bk bucket) string {
	if !bk.tokensKnown {
		if bk.sessions == 0 {
			return "0"
		}
		return "unknown"
	}
	return statsfmt.TokenCount(bk.tokens)
}

func (b *builder) dayRow(bk bucket) dayRow {
	return dayRow{
		Label: bucketLabel(bk, b.s.Window.Days), Sessions: statsfmt.CommaInt(int64(bk.sessions)),
		Spend: b.spendText(bk), Tokens: bucketTokens(bk),
	}
}

// bucketTitle is the tooltip of a bar: the day (or run of days), its spend and
// its sessions.
func (b *builder) bucketTitle(bk bucket, per int) string {
	label := bucketLabel(bk, b.s.Window.Days)
	switch {
	case bk.sessions == 0:
		return label + " · no sessions"
	case !bk.known():
		return label + " · " + plural(bk.sessions, "session") + " · cost unknown"
	case per > 1:
		return label + " · busiest day " + b.spendText(bk) + " · " + plural(bk.sessions, "session")
	}
	return label + " · " + b.spendText(bk) + " · " + plural(bk.sessions, "session")
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

// harnessClass is the color class of an agent, by the harness the archive names
// it with. An agent the page has no color for is neutral; the class is never
// made from the archive's text, only chosen from this list.
func harnessClass(harness string) string {
	if class, ok := harnessClasses[harness]; ok {
		return class
	}
	return "other"
}

// harnessClasses are the agents that have a color of their own, by harness.
var harnessClasses = map[string]string{"claude": "claude", "cursor": "cursor", "codex": "codex"}

// agentBar is the agents' share of sessions as one stacked bar: each agent's
// segment as wide as its share, the legend in the same order naming each with
// its share, so the colors are never the only way to tell them apart.
func (b *builder) agentBar() *agentBar {
	agents := b.s.Agents
	if len(agents) == 0 {
		return nil
	}
	bar := &agentBar{}
	used := 0.0
	var spoken []string
	for _, a := range agents {
		share := 0.0
		if finite(a.SessionShare) {
			share = math.Max(0, math.Min(a.SessionShare, 1-used))
		}
		label := clean(a.Label)
		class := "agent-" + harnessClass(a.Harness)
		text := statsfmt.Percent(a.SessionShare)
		bar.Segments = append(bar.Segments, agentSegment{
			Class: class, X: pct(used), W: pct(share),
			Title: label + " · " + plural(a.Sessions, "session") + " · " + text,
		})
		bar.Legend = append(bar.Legend, agentLegend{Class: class, Label: label, Share: text})
		spoken = append(spoken, label+" "+text)
		used += share
	}
	bar.Summary = "Sessions by agent: " + strings.Join(spoken, ", ") + "."
	return bar
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
	if !finite(share) || !finite(start) || share <= 0 {
		return arc{}
	}
	share = math.Min(share, 1)
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
		dash: dash, offset: offset, hasText: true, text: statsfmt.Percent(share),
		textX: num(donutCenter + donutRadius*math.Cos(angle)), textY: num(donutCenter + donutRadius*math.Sin(angle)),
	}
}

// tokens is the composition donut with its legend and the sentences that appear
// only when there is something to say.
func (b *builder) tokens() *tokenSection {
	c := b.s.Composition
	if c == nil || c.Total == 0 {
		return nil
	}
	t := &tokenSection{Total: statsfmt.TokenCount(c.Total)}
	parts := []struct {
		label string
		seg   stats.Segment
	}{{"Cache read", c.CacheRead}, {"Cache write", c.CacheWrite}, {"Input", c.FreshInput}, {"Output", c.Output}}
	circ := 2 * math.Pi * donutRadius
	start := 0.0
	for i, part := range parts {
		length := part.seg.Share * circ
		g := arcGeometry(part.seg.Share, start, circ)
		t.Segments = append(t.Segments, segment{
			Class: "seg" + strconv.Itoa(i+1), Label: part.label, Tokens: statsfmt.TokenCount(part.seg.Tokens), Share: statsfmt.Percent(part.seg.Share),
			Speak: fmt.Sprintf("%s: %s, %s of tokens", part.label, statsfmt.TokenCount(part.seg.Tokens), statsfmt.Percent(part.seg.Share)),
			Dash:  g.dash, Offset: g.offset, HasText: g.hasText, Text: g.text, TextX: g.textX, TextY: g.textY,
		})
		start += length
	}
	if c.ReasoningOfOutput != nil && *c.ReasoningOfOutput > 0 {
		t.Reasoning = "Output includes " + statsfmt.TokenCount(*c.ReasoningOfOutput) + " reasoning tokens."
	}
	if sub := b.s.Subagents; sub != nil {
		t.Subagents = fmt.Sprintf("Subagents used %s of tokens (%s) in %s.", statsfmt.Percent(sub.Share), statsfmt.TokenCount(sub.Tokens), plural(sub.Sessions, "run"))
	}
	return t
}

// callCount is "1 call" or "12 calls".
func callCount(n int64) string {
	if n == 1 {
		return "1 call"
	}
	return statsfmt.CommaInt(n) + " calls"
}
