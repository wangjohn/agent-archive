package statshtml

import (
	"math"
	"testing"
)

// A length that is not a number is no length at all.
func TestNonFiniteGeometryReadsAsZero(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := pct(v); got != "0%" {
			t.Errorf("pct(%v) = %q", v, got)
		}
		if got := num(v); got != "0" {
			t.Errorf("num(%v) = %q", v, got)
		}
	}
}
