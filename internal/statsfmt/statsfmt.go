// Package statsfmt writes the numbers of `agent-archive stats` as text: token
// counts, money, percentages and ordinals. The terminal screen and the web
// page both use it, so one number reads the same on both.
//
// It is pure: it imports only the archive's display cleaner and the standard
// library, and reads no clock, file or environment.
package statsfmt

import (
	"math"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// maxShownTokens and maxShownMoney are where a number is shown as a bound: a
// saturated sum, not a measurement.
const (
	maxShownTokens = 1e15
	maxShownMoney  = 1e12
)

// currencyLimit is how many characters of a currency code are shown; a price
// file only ever holds three letters.
const currencyLimit = 40

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// CommaInt is n with thousands separators: 3,204.
func CommaInt(n int64) string {
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

// TokenCount is a token total in its shortest form: 812, 4.9K, 61M, 1.2B.
// Under ten of a unit it keeps one decimal, beyond that none.
func TokenCount(n int64) string {
	if n >= maxShownTokens {
		// Sums saturate at the largest int64; no real usage is near this.
		return ">999T"
	}
	units := []struct {
		size   float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}}
	value := float64(n)
	larger := ""
	for _, u := range units {
		if value >= u.size {
			v := value / u.size
			if v < 9.95 {
				return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + u.suffix
			}
			rounded := math.Round(v)
			if rounded >= 1000 {
				// 999.6K reads as 1M, not 1000K.
				if larger == "" {
					return ">999T"
				}
				return "1" + larger
			}
			return strconv.FormatFloat(rounded, 'f', 0, 64) + u.suffix
		}
		larger = u.suffix
	}
	return strconv.FormatInt(n, 10)
}

// Money is an amount of currency. USD is "$" and any other currency its code
// and a space. An amount from ten up has no cents unless precise. An amount
// that is not a number reads "n/a".
func Money(currency string, amount float64, precise bool) string {
	symbol := "$"
	if currency != "" && currency != "USD" {
		symbol = archive.DisplayLine(currency)
		if runes := []rune(symbol); len(runes) > currencyLimit {
			symbol = string(runes[:currencyLimit-1]) + "…"
		}
		symbol += " "
	}
	if math.IsNaN(amount) {
		return "n/a"
	}
	if amount >= maxShownMoney {
		return symbol + ">999B"
	}
	if !precise && amount >= 10 {
		return symbol + CommaInt(int64(math.Round(amount)))
	}
	whole, cents, _ := strings.Cut(strconv.FormatFloat(amount, 'f', 2, 64), ".")
	n, _ := strconv.ParseInt(whole, 10, 64)
	return symbol + CommaInt(n) + "." + cents
}

// Percent is a share (0 to 1) as a whole percentage, "<1%" for a nonzero
// share under half a percent, and "n/a" for one that is not a number.
func Percent(share float64) string {
	if !finite(share) {
		return "n/a"
	}
	if share > 0 && share < 0.005 {
		return "<1%"
	}
	return strconv.FormatFloat(math.Round(share*100), 'f', 0, 64) + "%"
}

// RatePercent is a rate (0 to 1) with one decimal under ten percent.
func RatePercent(rate float64) string {
	if !finite(rate) {
		return "n/a"
	}
	if rate > 0 && rate < 0.1 {
		return strconv.FormatFloat(rate*100, 'f', 1, 64) + "%"
	}
	return Percent(rate)
}

// Ordinal is n with its English suffix: 1st, 2nd, 3rd, 4th.
func Ordinal(n int) string {
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
