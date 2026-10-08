package archive

import (
	"reflect"
	"testing"
	"time"
)

func FuzzSessionLabelEvidence(f *testing.F) {
	for _, seed := range [][3]string{
		{"01900000-0000-7000-8000-000000000001", "codex-files-159.2-v1", "Prior verified name"},
		{"fixture-thread", "fixture-provider-v1", "Name\nwith whitespace"},
		{"fixture-thread", "fixture-provider-v1", " \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"fixture-thread", "AKIAABCDEFGHIJKLMNOP", "Invented name"},
		{"fixture-thread", "/private/native/path", "Invented name"},
		{"fixture-thread", "fixture-provider-v1", "Name sk-proj-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, id, contract, name string) {
		filtered, keep := FilterSessionLabel(SessionLabel{NativeID: id, State: SessionLabelPresent, Name: name, Source: SessionLabelDatabase, Contract: contract})
		if !keep {
			return
		}
		evidence, _, err := FilterSupplementalEvidence([]SupplementalEvidence{filtered.Evidence(at, "fixture")})
		if err != nil || len(evidence) != 1 {
			t.Fatalf("filtered typed evidence failed persistence: %v", err)
		}
		bundle := SourceBundle{SchemaVersion: SourceSchemaVersion, NativeSessionID: filtered.NativeID, Capture: SourceCapture{Harness: Harness{Name: "fixture"}}, SupplementalEvidence: evidence}
		if err := ValidateSessionLabels(bundle); err != nil {
			t.Fatal(err)
		}
		restored, observed, ok := CurrentSessionLabel(bundle)
		if !ok || !reflect.DeepEqual(restored, filtered) || !observed.Equal(at) {
			t.Fatal("repeated filtering changed typed evidence")
		}
		bundle.NativeSessionID += "-different-owner"
		if _, _, ok := CurrentSessionLabel(bundle); ok || ValidateSessionLabels(bundle) == nil {
			t.Fatal("label crossed owning source identity")
		}
	})
}
