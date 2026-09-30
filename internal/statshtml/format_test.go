package statshtml

import (
	"math"
	"testing"
)

// The page formats numbers with its own copy of the terminal screen's
// formatters (this package may not import internal/cli), so it pins the same
// table the screen's TestStatsNumberFormats does; a change to one that is not
// made to the other fails a test. TestStatsPageAgreesWithTheTerminalAndJSON
// compares whole pages.
func TestNumberFormatsMatchTheTerminalScreen(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{
		0: "0", 812: "812", 999: "999", 1000: "1K", 1234: "1.2K", 9949: "9.9K", 9950: "10K", 51_400: "51K",
		999_499: "999K", 999_500: "1M", 999_999: "1M", 1_000_000: "1M", 4_900_000: "4.9M", 61_000_000: "61M",
		999_600_000: "1B", 1_200_000_000: "1.2B", 3_000_000_000_000: "3T", 999_500_000_000_000: ">999T",
		math.MaxInt64: ">999T",
	} {
		if got := tokenCount(n); got != want {
			t.Errorf("tokenCount(%d) = %q, want %q", n, got, want)
		}
	}
	for _, tc := range []struct {
		amount  float64
		precise bool
		want    string
	}{
		{0, false, "$0.00"}, {0.126, false, "$0.13"}, {5.69, false, "$5.69"}, {9.999, false, "$10.00"}, {59.4, false, "$59"},
		{612.5, false, "$613"}, {41.2, true, "$41.20"}, {1204.5, false, "$1,205"}, {1204.5, true, "$1,204.50"},
		{1e13, false, "$>999B"}, {math.MaxFloat64, true, "$>999B"},
	} {
		if got := money("USD", tc.amount, tc.precise); got != tc.want {
			t.Errorf("money(%v, precise=%v) = %q, want %q", tc.amount, tc.precise, got, tc.want)
		}
	}
	if got := money("EUR", 12.4, false); got != "EUR 12" {
		t.Errorf("money in EUR = %q", got)
	}
	for share, want := range map[float64]string{0: "0%", 0.004: "<1%", 0.005: "1%", 0.184: "18%", 0.995: "100%", 1: "100%"} {
		if got := percent(share); got != want {
			t.Errorf("percent(%v) = %q, want %q", share, got, want)
		}
	}
	for rate, want := range map[float64]string{0: "0%", 0.041: "4.1%", 0.0995: "10.0%", 0.25: "25%"} {
		if got := ratePercent(rate); got != want {
			t.Errorf("ratePercent(%v) = %q, want %q", rate, got, want)
		}
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 3204: "3,204", 1234567: "1,234,567", -5000: "-5,000"} {
		if got := commaInt(n); got != want {
			t.Errorf("commaInt(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// A number that is not a number is never shown as one.
func TestNonFiniteNumbersReadAsUnavailable(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := percent(v); got != "n/a" {
			t.Errorf("percent(%v) = %q", v, got)
		}
		if got := ratePercent(v); got != "n/a" {
			t.Errorf("ratePercent(%v) = %q", v, got)
		}
		// A length that is not a number is no length at all.
		if got := pct(v); got != "0%" {
			t.Errorf("pct(%v) = %q", v, got)
		}
		if got := num(v); got != "0" {
			t.Errorf("num(%v) = %q", v, got)
		}
	}
	if got := money("USD", math.NaN(), false); got != "n/a" {
		t.Errorf("money(NaN) = %q", got)
	}
}
