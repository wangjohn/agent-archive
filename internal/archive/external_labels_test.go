package archive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionLabelFiltersBeforeEvidenceAndFingerprint(t *testing.T) {
	label := SessionLabel{NativeID: "01900000-0000-7000-8000-000000000001", State: "present", Name: "Name\x1b\n sk-proj-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", Source: "database", Contract: SessionLabelContract}
	filtered, ok := FilterSessionLabel(label)
	if !ok || strings.Contains(filtered.Name, "sk-proj-") || strings.ContainsRune(filtered.Name, '\x1b') {
		t.Fatalf("%+v %v", filtered, ok)
	}
	e := filtered.Evidence(time.Now())
	out, _, err := FilterSupplementalEvidence([]SupplementalEvidence{e})
	if err != nil || len(out) != 1 {
		t.Fatalf("%v %v", out, err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "sk-proj-") {
		t.Fatal("raw secret retained")
	}
	if filtered.Fingerprint() != label.Fingerprint() {
		t.Fatal("fingerprint used raw name")
	}
	e.Payload["unknown"] = "private"
	if _, _, err := FilterSupplementalEvidence([]SupplementalEvidence{e}); err == nil {
		t.Fatal("unknown native response field retained")
	}
}

func TestSessionLabelReplacementPreservesUnchangedTimeAndOwningID(t *testing.T) {
	label := SessionLabel{NativeID: "01900000-0000-7000-8000-000000000001", State: "present", Name: "Name", Source: "index", Contract: SessionLabelContract}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := label.Evidence(at)
	out := MergeSupplementalEvidence([]SupplementalEvidence{first}, []SupplementalEvidence{label.Evidence(at.Add(time.Hour))})
	if len(out) != 1 || !out[0].ObservedAt.Equal(at) {
		t.Fatal("unchanged name renewed observation")
	}
	label.Name = "Renamed"
	out = MergeSupplementalEvidence(out, []SupplementalEvidence{label.Evidence(at.Add(time.Hour))})
	if len(out) != 1 || out[0].Payload["name"] != "Renamed" {
		t.Fatal("old name history accumulated")
	}
	bundle := SourceBundle{NativeSessionID: "01900000-0000-7000-8000-000000000002", Capture: SourceCapture{Harness: Harness{Name: "codex"}}, SupplementalEvidence: out}
	if ValidateSessionLabels(bundle) == nil {
		t.Fatal("another session's label admitted")
	}
}
