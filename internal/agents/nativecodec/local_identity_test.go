package nativecodec

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Identity facts omit excluded records and preserve only retained sanitized IDs.
func TestLocalIdentityUsesOnlySafeRetainedEvidence(t *testing.T) {
	t.Parallel()
	input := `{"type":"unknown","sessionId":"omitted"}` + "\n" + `{"type":"user","sessionId":"sk-abcdefghijklmnopqrstuv","message":{"role":"user","content":"inspect"}}` + "\n"
	f, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got := (ClaudeAdapter{}).LocalIdentity(f, "omitted.jsonl").ID; got != "[REDACTED]" {
		t.Fatalf("identity %q", got)
	}
	if len(f.LocalIdentity.Candidates) != 1 || f.LocalIdentity.Candidates[0] != "[REDACTED]" {
		t.Fatalf("safe fact %+v", f.LocalIdentity)
	}
	if len(f.SessionIDs) != 2 {
		t.Fatalf("ownership observations changed: %v", f.SessionIDs)
	}
}

// Retained metadata remains authoritative, while mixed copied identities prefer
// the file stem and never use raw ownership-only observations.
func TestLocalIdentityPreference(t *testing.T) {
	t.Parallel()
	f := archive.FilteredTranscript{SessionIDs: []string{"private"}, LocalIdentity: archive.NativeSessionIdentity{Candidates: []string{"original", "resumed"}}}
	if got := (ClaudeAdapter{}).LocalIdentity(f, "resumed.jsonl").ID; got != "resumed" {
		t.Fatal(got)
	}
	if got := (ClaudeAdapter{}).LocalIdentity(f, "other.jsonl").ID; got != "original" {
		t.Fatal(got)
	}
	f.LocalIdentity.ID = "meta-id"
	if got := (CodexAdapter{}).LocalIdentity(f, "resumed.jsonl").ID; got != "meta-id" {
		t.Fatal(got)
	}
}
