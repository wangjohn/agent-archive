package archive

import (
	"bytes"
	"strings"
	"testing"
)

func TestPairingBundleRedactionBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"Paste aa-pair1:synthetic_ABC-123 now.", "Paste [REDACTED] now."},
		{"aa-pair1:a", "[REDACTED]"},
		{"AA-PAIR1:abc+/==", "[REDACTED]"},
		{"(aa-pair1:ab_cd-ef), next", "([REDACTED]), next"},
		{`\"aa-pair1:abc\"`, `\"[REDACTED]\"`},
		{"  12→aa-pair1:abc\n  13→next", "  12→[REDACTED]\n  13→next"},
		{"aa-pair1:abc\r\naa-pair1:def", "[REDACTED]\r\n[REDACTED]"},
		{"aa-pair1:abc!tail", "[REDACTED]!tail"},
		{"aa-pair1:abc def", "[REDACTED] def"},
		{"aa-pair1:", "aa-pair1:"},
		{"xaa-pair1:abc _aa-pair1:abc", "xaa-pair1:abc _aa-pair1:abc"},
		{"aa-pair2:abc", "aa-pair2:abc"},
		{"acorn cable lemon orbit river violet", "acorn cable lemon orbit river violet"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, hit := redactSensitive(tc.in)
			if got != tc.want || hit != (tc.in != tc.want) {
				t.Fatalf("got %q (%v), want %q", got, hit, tc.want)
			}
			if again := RedactText(got); again != got {
				t.Fatalf("not idempotent: %q", again)
			}
			checkGatedRedactionMatchesWhole(t, strings.Repeat("ordinary text\n", 1000)+tc.in)
		})
	}
}

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
