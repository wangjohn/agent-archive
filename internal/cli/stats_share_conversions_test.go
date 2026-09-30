package cli

import (
	"math"
	"testing"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// The cells of a bar and the level of a chart bar are computed from floats,
// and a float that is NaN, infinite or out of range converts to an integer as
// the CPU pleases (arm64 saturates, amd64 gives the most negative integer): a
// wrong count of cells would draw a bar of a negative length and panic. Any
// number gives the same, in-range answer on every platform.
func TestStatsBarsAndChartLevelsSurviveAnyFloat(t *testing.T) {
	t.Parallel()
	shares := []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, -1e300, 1e300, math.MaxFloat64, 0, 0.5, 1}
	for _, share := range shares {
		for _, width := range []int{0, 1, 6, 30} {
			if n := barCells(share, width); n < 0 || n > width {
				t.Errorf("barCells(%v, %d) = %d, outside 0 to %d", share, width, n, width)
			}
		}
	}
	for _, spend := range []float64{math.NaN(), math.Inf(1), math.MaxFloat64, 1e300, 5} {
		for _, peak := range []float64{math.NaN(), math.Inf(1), math.MaxFloat64, 1e-300, 5} {
			cell := spendCellOf([]stats.Day{{Sessions: 1, Cost: usd(spend)}}, peak)
			if cell.level < 0 || cell.level > chartLevels {
				t.Errorf("a chart bar of %v against a peak of %v is at level %d, outside 0 to %d", spend, peak, cell.level, chartLevels)
			}
			if cell.level != 0 && cell.level < chartMinLevel {
				t.Errorf("a chart bar of %v against a peak of %v is at level %d, under the minimum %d", spend, peak, cell.level, chartMinLevel)
			}
		}
	}
	if got := barCells(math.NaN(), 30); got != 0 {
		t.Errorf("a share that is not a number fills %d cells", got)
	}
}
