package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthProducerWritesBoundedContentFreeEvidence(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h := Health{Enabled: true, LastAttempt: at, Pending: true, Outcomes: map[string]int{"unsupported_producer": 1}}
	if e := writeHealth(home, h); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(home, "discovery-health.json"))
	if e != nil || len(raw) > maxHealthBytes {
		t.Fatalf("summary size/error %d %v", len(raw), e)
	}
	var saved healthSummary
	if e = json.Unmarshal(raw, &saved); e != nil || saved.Version != 1 || !saved.Health.LastAttempt.Equal(at) || !saved.Health.Pending {
		t.Fatalf("saved evidence %#v %v", saved, e)
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
	h.LastAttempt = at
	h.Outcomes = map[string]int{strings.Repeat("x", 65): 1}
	if writeHealth(home, h) == nil {
		t.Fatal("unbounded outcome accepted")
	}
}
