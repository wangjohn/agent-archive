package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthSummaryIsBoundedIndependentAndRejectsUnknownEvidence(t *testing.T) {
	home := t.TempDir()
	if _, found, e := ReadHealth(home); e != nil || found {
		t.Fatal("missing health became success")
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h := Health{Enabled: true, LastAttempt: at, Pending: true, Outcomes: map[string]int{"unsupported_producer": 1}}
	if e := writeHealth(home, h); e != nil {
		t.Fatal(e)
	}
	_ = os.WriteFile(filepath.Join(home, "discovery-catalog.json"), []byte("corrupt catalog irrelevant"), 0600)
	got, found, e := ReadHealth(home)
	if e != nil || !found || !got.Pending || !got.LastAttempt.Equal(at) {
		t.Fatalf("summary %#v %t %v", got, found, e)
	}
	b, _ := os.ReadFile(filepath.Join(home, "discovery-health.json"))
	if len(b) > maxHealthBytes || strings.Contains(string(b), "catalog irrelevant") {
		t.Fatal("health retained source catalog")
	}
	h.Errors = []string{"/private/project/native-id"}
	if writeHealth(home, h) == nil {
		t.Fatal("raw diagnostic accepted")
	}
	h.Errors = nil
	h.LastAttempt = time.Time{}
	if writeHealth(home, h) == nil {
		t.Fatal("zero attempt became success")
	}
	_ = os.WriteFile(filepath.Join(home, "discovery-health.json"), []byte(strings.Repeat("x", maxHealthBytes+1)), 0600)
	if _, found, e = ReadHealth(home); e == nil || found {
		t.Fatal("oversize health accepted")
	}
}

func BenchmarkBoundedHealthSummaryRead(b *testing.B) {
	home := b.TempDir()
	h := Health{Enabled: true, LastAttempt: time.Now().UTC(), LastReconciled: time.Now().UTC(), Pending: true, Outcomes: map[string]int{"supported": 128, "unsupported_producer": 128}, Errors: []string{"retry_limit"}}
	if e := writeHealth(home, h); e != nil {
		b.Fatal(e)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "discovery-health.json"))
	b.ReportMetric(float64(len(raw)), "summary_bytes")
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		_, found, e := ReadHealth(home)
		if e != nil || !found {
			b.Fatal(e)
		}
	}
}

func TestUnreadableHealthNeverLooksLikeSuccessfulCoverage(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "discovery-health.json"), []byte(`{"private_prompt":"SYNTHETIC_BODY","health":`), 0600); err != nil {
		t.Fatal(err)
	}
	health, found, err := ReadHealth(home)
	if err == nil || found || health.Supported || !health.LastReconciled.IsZero() {
		t.Fatalf("bad catalog looked healthy: %+v", health)
	}
}
