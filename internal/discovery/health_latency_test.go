package discovery

import (
	"os"
	"sort"
	"testing"
	"time"
)

// This opt-in check measures reader latency, not whole-status latency or cold
// storage. Freshly written files remain in the OS page cache.
func TestBoundedHealthReaderLatencyDistribution(t *testing.T) {
	if os.Getenv("AGENT_ARCHIVE_MEASURE_HEALTH_LATENCY") != "1" {
		t.Skip("opt-in latency measurement")
	}
	h := Health{Enabled: true, LastAttempt: time.Now().UTC(), Pending: true, Outcomes: map[string]int{"supported": 128, "unsupported_producer": 128}, Errors: []string{"retry_limit"}}
	firstOpen := make([]time.Duration, 128)
	var warmHome string
	for i := range firstOpen {
		home := t.TempDir()
		if err := writeHealth(home, h); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, found, err := ReadHealth(home)
		firstOpen[i] = time.Since(started)
		if err != nil || !found {
			t.Fatalf("first open: %t %v", found, err)
		}
		warmHome = home
	}
	warm := make([]time.Duration, 2048)
	for i := range warm {
		started := time.Now()
		_, found, err := ReadHealth(warmHome)
		warm[i] = time.Since(started)
		if err != nil || !found {
			t.Fatalf("warm read: %t %v", found, err)
		}
	}
	p95 := func(samples []time.Duration) time.Duration {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[(95*len(samples)+99)/100-1]
	}
	warmP95, firstP95 := p95(warm), p95(firstOpen)
	t.Logf("health reader warm p95=%s n=%d; first-open page-cache-warm p95=%s n=%d; true cold OS-cache p95 unavailable", warmP95, len(warm), firstP95, len(firstOpen))
	if warmP95 >= 10*time.Millisecond {
		t.Errorf("warm reader p95 %s exceeds 10ms target", warmP95)
	}
}
