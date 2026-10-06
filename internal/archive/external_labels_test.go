package archive

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSessionLabelFiltersBeforeEvidenceAndFingerprint(t *testing.T) {
	label := SessionLabel{NativeID: "01900000-0000-7000-8000-000000000001", State: SessionLabelPresent, Name: "Name\x1b\n sk-proj-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", Source: SessionLabelDatabase, Contract: "synthetic-label-v1"}
	filtered, ok := FilterSessionLabel(label)
	if !ok || strings.Contains(filtered.Name, "sk-proj-") || strings.ContainsRune(filtered.Name, '\x1b') {
		t.Fatalf("%+v %v", filtered, ok)
	}
	e := filtered.Evidence(time.Now(), "codex")
	if got, ok := labelFromEvidence(e); !ok || got != filtered {
		t.Fatalf("typed label failed direct in-memory evidence round trip: %+v %v", got, ok)
	}
	if _, ok := e.Payload["state"].(string); !ok {
		t.Fatal("state payload is not a plain string")
	}
	if _, ok := e.Payload["source"].(string); !ok {
		t.Fatal("source payload is not a plain string")
	}
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
	label := SessionLabel{NativeID: "01900000-0000-7000-8000-000000000001", State: SessionLabelPresent, Name: "Name", Source: SessionLabelIndex, Contract: "synthetic-label-v1"}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := label.Evidence(at, "codex")
	out := MergeSupplementalEvidence([]SupplementalEvidence{first}, []SupplementalEvidence{label.Evidence(at.Add(time.Hour), "codex")})
	if len(out) != 1 || !out[0].ObservedAt.Equal(at) {
		t.Fatal("unchanged name renewed observation")
	}
	label.Name = "Renamed"
	out = MergeSupplementalEvidence(out, []SupplementalEvidence{label.Evidence(at.Add(time.Hour), "codex")})
	if len(out) != 1 || out[0].Payload["name"] != "Renamed" {
		t.Fatal("old name history accumulated")
	}
	bundle := SourceBundle{NativeSessionID: "01900000-0000-7000-8000-000000000002", Capture: SourceCapture{Harness: Harness{Name: "codex"}}, SupplementalEvidence: out}
	if ValidateSessionLabels(bundle) == nil {
		t.Fatal("another session's label admitted")
	}
}

func TestPriorSessionLabelContractRemainsReadable(t *testing.T) {
	label := SessionLabel{NativeID: "01900000-0000-7000-8000-000000000001", State: SessionLabelPresent, Name: "Prior verified name", Source: SessionLabelIndex, Contract: "codex-files-159.2-v1"}
	bundle := SourceBundle{SchemaVersion: SourceSchemaVersion, NativeSessionID: label.NativeID, Capture: SourceCapture{Harness: Harness{Name: "codex"}}, SupplementalEvidence: []SupplementalEvidence{label.Evidence(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "codex")}}
	if err := ValidateSessionLabels(bundle); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := CurrentSessionLabel(bundle); !ok || got.Name != label.Name {
		t.Fatalf("prior contract lost offline name: %+v %v", got, ok)
	}
}

func TestSessionLabelGenericOwnershipAndOpaqueContractPrivacy(t *testing.T) {
	label := SessionLabel{NativeID: "claude-thread-1", State: SessionLabelPresent, Name: "Verified name", Source: SessionLabelIndex, Contract: "synthetic-provider-v1"}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bundle := SourceBundle{SchemaVersion: SourceSchemaVersion, NativeSessionID: label.NativeID, Capture: SourceCapture{Harness: Harness{Name: "claude"}}, SupplementalEvidence: []SupplementalEvidence{label.Evidence(at, "claude")}}
	if err := ValidateSessionLabels(bundle); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := CurrentSessionLabel(bundle); !ok || got.Name != label.Name {
		t.Fatal("generic owning label lost")
	}
	bundle.SupplementalEvidence[0] = label.Evidence(at, "codex")
	if ValidateSessionLabels(bundle) == nil {
		t.Fatal("another harness's evidence changed an owner")
	}
	for _, contract := range []string{"/private/native/path", "raw native error", strings.Repeat("a", 65), "sk-proj-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"} {
		label.Contract = contract
		if _, ok := FilterSessionLabel(label); ok {
			t.Fatalf("private or unbounded contract accepted: %q", contract)
		}
	}
}
