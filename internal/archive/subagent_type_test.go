package archive

import (
	"strings"
	"testing"
	"time"
)

// A subagent type reaches a capture gap's detail, so only a short name of
// letters, digits, and "_.:-" is kept; anything else is dropped whole.
func TestSanitizeSubagentTypeKeepsOnlyShortPlainNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"Explore", "Explore"},
		{"general-purpose", "general-purpose"},
		{"my-plugin:reviewer", "my-plugin:reviewer"},
		{"v1.2_agent", "v1.2_agent"},
		{strings.Repeat("a", MaxSubagentTypeLength), strings.Repeat("a", MaxSubagentTypeLength)},
		{strings.Repeat("a", MaxSubagentTypeLength+1), ""},
		{"", ""},
		{"has space", ""},
		{"slash/name", ""},
		{"quote\"", ""},
		{"newline\n", ""},
		{"Érudit", ""},
		{"<system-reminder>", ""},
	} {
		if got := SanitizeSubagentType(tc.in); got != tc.want {
			t.Errorf("SanitizeSubagentType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Capture gap evidence passes the same filter as every other evidence: its
// detail is redacted, and a payload key other than code and detail cannot be
// added through it.
func TestNewCaptureGapEvidenceIsFiltered(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	secret := "AKIA" + "IOSFODNN7EXAMPLE"
	evidence, err := NewCaptureGapEvidence("subagent_transcript_never_written", "type "+secret, "collector:subagent-expiry", at)
	if err != nil {
		t.Fatal(err)
	}
	detail, _ := evidence.Payload["detail"].(string)
	if evidence.Kind != EvidenceKindCaptureGap || evidence.Payload["code"] != "subagent_transcript_never_written" || strings.Contains(detail, secret) || !strings.Contains(detail, "[REDACTED]") {
		t.Fatalf("evidence=%#v", evidence)
	}
	if _, err := NewCaptureGapEvidence("", "detail", "collector:subagent-expiry", at); err == nil {
		t.Fatal("a gap without a code was built")
	}
	if _, err := NewCaptureGapEvidence("code", "detail", "", at); err == nil {
		t.Fatal("a gap without provenance was built")
	}
}
