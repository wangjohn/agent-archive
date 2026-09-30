package statshtml

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// maxShownTokens and maxShownMoney are where a number is shown as a bound: a
// saturated sum, not a measurement.
const (
	maxShownTokens = 1e15
	maxShownMoney  = 1e12
)

// nameLimit is how many characters of a project, model, skill or MCP server
// name the page shows; longer names are cut with an ellipsis.
const nameLimit = 40

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

// tokenCount is a token total in its shortest form: 812, 4.9K, 61M, 1.2B.
func tokenCount(n int64) string {
	if n >= maxShownTokens {
		return ">999T"
	}
	units := []struct {
		size   float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}}
	value := float64(n)
	for _, u := range units {
		if value >= u.size {
			v := value / u.size
			if v < 9.95 {
				return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + u.suffix
			}
			return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// money is an amount of currency; an amount from ten up has no cents unless
// precise.
func money(currency string, amount float64, precise bool) string {
	symbol := "$"
	if currency != "" && currency != "USD" {
		symbol = plain(currency) + " "
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

// plural is "1 session" or "3 sessions".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// ordinal is 1st, 2nd, 3rd, 4th.
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

// pct is a share (0 to 1) as an SVG length in percent, clamped to the range
// and rounded to two decimals so the page is byte-stable.
func pct(share float64) string {
	share = math.Max(0, math.Min(1, share))
	return strconv.FormatFloat(math.Round(share*10000)/100, 'f', -1, 64) + "%"
}

// num is a coordinate rounded to two decimals, as short as it can be.
func num(v float64) string {
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
	text := money(currency, *usd, precise)
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
// percent"). "new" stands for a previous period without any, and both are
// empty when either side is unknown.
func deltaText(m stats.Measure) (glyph, spoken string) {
	if m.Value == nil || m.Previous == nil {
		return "", ""
	}
	if m.ChangePct == nil {
		if *m.Previous == 0 && *m.Value > 0 {
			return "new", "new, nothing in the previous period"
		}
		return "", ""
	}
	p := math.Round(*m.ChangePct)
	switch {
	case p > 0:
		return "▲ " + commaInt(int64(p)) + "%", "up " + commaInt(int64(p)) + " percent"
	case p < 0:
		return "▼ " + commaInt(int64(-p)) + "%", "down " + commaInt(int64(-p)) + " percent"
	}
	return "no change", "no change"
}

func measureCount(m stats.Measure) string {
	if m.Value == nil {
		return "unknown"
	}
	return commaInt(int64(math.Round(*m.Value)))
}

func measureTokens(m stats.Measure) string {
	if m.Value == nil {
		return "unknown"
	}
	return tokenCount(int64(math.Round(*m.Value)))
}

func ordinalHeaviest(rank int) string {
	if rank <= 1 {
		return "heaviest"
	}
	return ordinal(rank) + "-heaviest"
}

// driversText says what likely made the costliest session costly.
func driversText(c *stats.CostliestSession) string {
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
