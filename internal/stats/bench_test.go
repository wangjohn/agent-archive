package stats

import (
	"math/rand/v2"
	"testing"
	"time"
)

// A long-time user's archive: 50,000 sessions. Compute must stay a fraction of
// a second and allocate in proportion to the sessions, since the command runs
// it on every `stats`.
func BenchmarkCompute50kSessions(b *testing.B) {
	sessions, _ := randomArchive(rand.New(rand.NewPCG(7, 7)), 50_000)
	o := Options{Now: time.Date(2026, time.September, 20, 12, 0, 0, 0, newYork), Location: newYork, Days: 400, By: GroupWeek}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		Compute(sessions, o)
	}
}
