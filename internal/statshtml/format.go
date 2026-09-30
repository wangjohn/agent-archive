package statshtml

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

// nameLimit is how many characters of a project, model, skill or MCP server
// name the page shows; longer names are cut with an ellipsis.
const nameLimit = 40

// finite is whether v is a number a page can show: neither NaN nor infinite.
// The engine never returns one (its JSON could not carry it), but the page
// draws whatever it is given, and "NaN%" or a negative length is never
// acceptable in it.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// plain is text read from the archive that is shown whole: cleaned as clean
// does but not shortened (a price table's version, a time zone name, a note).
func plain(text string) string { return archive.DisplayLine(text) }

// clean is a name read from the archive as it may be shown: no control,
// bidirectional-override or line-separator characters, whitespace collapsed
// to single spaces, invalid UTF-8 replaced, and at most nameLimit characters.
// The template escapes the result for HTML; clean is what keeps a name that
// smuggles in a line break or a right-to-left override from rearranging the
// page's text.
func clean(name string) string {
	text := plain(name)
	runes := []rune(text)
	if len(runes) > nameLimit {
		return string(runes[:nameLimit-1]) + "…"
	}
	return text
}

// plural is "1 session" or "3 sessions".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// pct is a share (0 to 1) as an SVG length in percent, clamped to the range
// and rounded to two decimals so the page is byte-stable.
func pct(share float64) string {
	if !finite(share) {
		share = 0
	}
	share = math.Max(0, math.Min(1, share))
	return strconv.FormatFloat(math.Round(share*10000)/100, 'f', -1, 64) + "%"
}

// num is a coordinate rounded to two decimals, as short as it can be.
func num(v float64) string {
	if !finite(v) {
		return "0"
	}
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// dayLabel is a "2006-01-02" date as "Sep 17", with the year when the window
// is long enough for it to be ambiguous.
func dayLabel(date string, windowDays int) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return clean(date)
	}
	if windowDays > 300 {
		return t.Format("Jan 2 2006")
	}
	return t.Format("Jan 2")
}

// costFlags records which qualifiers the costs shown carried, so the footer
// can explain them: ~ for an approximate cost and + for one that leaves out
// unpriced tokens.
type costFlags struct {
	approx  bool
	partial bool
}

// costText is an estimated cost: "unpriced" when there is no amount. A leading
// ~ marks an approximate cost and a trailing + one that leaves out unpriced
// tokens.
func (f *costFlags) costText(currency string, usd *float64, approximate, partial, precise bool) string {
	if usd == nil {
		return "unpriced"
	}
	text := statsfmt.Money(currency, *usd, precise)
	if approximate {
		text = "~" + text
		f.approx = true
	}
	if partial {
		text += "+"
		f.partial = true
	}
	return text
}

// deltaText is a measure's change against the previous period as a glyph and
// a percentage ("▲ 18%"), and the same in words for screen readers ("up 18
// percent"), with its direction ("up", "down" or "flat"). All three are empty
// when there is no previous period to compare with (nothing in it, or a side
// that is unknown): a change against nothing is not a change, so the page says
// nothing rather than "new".
func deltaText(m stats.Measure) (glyph, spoken, dir string) {
	if m.Value == nil || m.Previous == nil || !finite(*m.Value) || !finite(*m.Previous) || *m.Previous <= 0 {
		return "", "", ""
	}
	if m.ChangePct == nil || !finite(*m.ChangePct) {
		return "", "", ""
	}
	p := math.Round(*m.ChangePct)
	switch {
	case p > 0:
		return "▲ " + statsfmt.CommaInt(int64(p)) + "%", "up " + statsfmt.CommaInt(int64(p)) + " percent", "up"
	case p < 0:
		return "▼ " + statsfmt.CommaInt(int64(-p)) + "%", "down " + statsfmt.CommaInt(int64(-p)) + " percent", "down"
	}
	return "no change", "no change", "flat"
}

func measureCount(m stats.Measure) string {
	if m.Value == nil || !finite(*m.Value) {
		return "unknown"
	}
	return statsfmt.CommaInt(int64(math.Round(*m.Value)))
}

func measureTokens(m stats.Measure) string {
	if m.Value == nil || !finite(*m.Value) {
		return "unknown"
	}
	return statsfmt.TokenCount(int64(math.Round(*m.Value)))
}

func ordinalHeaviest(rank int) string {
	if rank <= 1 {
		return "heaviest"
	}
	return statsfmt.Ordinal(rank) + "-heaviest"
}

// driversText says what likely made the costliest session costly: the drivers
// the engine named, in words, with the subagents' number and the cache-hit
// rate where they are known.
func driversText(drivers []string, subagents int, cacheHitRate *float64) string {
	var parts []string
	if slices.Contains(drivers, stats.DriverLongContext) {
		parts = append(parts, "long context")
	}
	if slices.Contains(drivers, stats.DriverSubagents) {
		parts = append(parts, plural(subagents, "subagent"))
	}
	if slices.Contains(drivers, stats.DriverLowCacheHit) {
		if cacheHitRate != nil {
			parts = append(parts, statsfmt.Percent(*cacheHitRate)+" cache hit")
		} else {
			parts = append(parts, "low cache hit")
		}
	}
	return strings.Join(parts, ", ")
}
