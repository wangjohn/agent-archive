package nativecodec

import (
	"bytes"
	"strings"
	"testing"
)

func TestFilterRedactsPairingBundlesAtEveryRetainedDepth(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude", "codex", "cursor"} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			adapter := adapterForFixture(t, harness+"-pairing-bundles.jsonl")
			filtered, err := adapter.FilterJSONL(bytes.NewReader(fixture(t, harness+"-pairing-bundles.jsonl")))
			if err != nil {
				t.Fatal(err)
			}
			joined := string(bytes.Join(filtered.Records, []byte("\n")))
			for _, secret := range []string{"syntheticProse", "syntheticTool", "syntheticNested", "syntheticDisplayed"} {
				if strings.Contains(joined, secret) {
					t.Errorf("retained %q: %s", secret, joined)
				}
			}
			if strings.Count(joined, "[REDACTED]") != 4 {
				t.Errorf("expected all four retained contexts: %s", joined)
			}
			if !hasGap(filtered.Gaps, "sensitive_content_redacted") {
				t.Errorf("missing redaction gap: %#v", filtered.Gaps)
			}
			again, err := adapter.FilterJSONL(bytes.NewReader(bytes.Join(filtered.Records, []byte("\n"))))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.Join(again.Records, []byte("\n")), bytes.Join(filtered.Records, []byte("\n"))) {
				t.Fatal("refilter changed retained records")
			}
		})
	}
}
