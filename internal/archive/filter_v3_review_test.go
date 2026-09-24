package archive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A block nested inside a block of the same kind must extend the outer block,
// not end it early: the tail of the outer block is still injected text.
// Multiple blocks of different kinds in one string are all removed and the
// text between them is kept.
func TestFilterV3StripsNestedAndMultipleInstructionBlocks(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"nested same kind": {
			in:   "before <system-reminder>outer <system-reminder>inner</system-reminder> tail-of-outer</system-reminder> after",
			want: "before  after",
		},
		"multiple kinds": {
			in:   "a <system-reminder>x</system-reminder> b <user_instructions>y</user_instructions> c <environment_context><cwd>/w</cwd></environment_context> d",
			want: "a  b  c  d",
		},
		"different kind nested inside": {
			in:   "p <user_instructions>u <system-reminder>s</system-reminder> v</user_instructions> q",
			want: "p  q",
		},
		"nested and unterminated": {
			in:   "keep <system-reminder>outer <system-reminder>inner</system-reminder> never closed",
			want: "keep",
		},
		"attributes on the tag": {
			in:   "keep <system-reminder kind=\"memory\">m</system-reminder> too",
			want: "keep  too",
		},
	}
	for name, tc := range cases {
		hit, got := stripInjectedInstructions(tc.in)
		if !hit || got != tc.want {
			t.Errorf("%s: stripInjectedInstructions(%q) = (%v, %q), want %q", name, tc.in, hit, got, tc.want)
		}
		for _, leaked := range []string{"outer", "inner", "tail-of-outer", "never closed", "<system-reminder", "<user_instructions", "<environment_context"} {
			if strings.Contains(got, leaked) {
				t.Errorf("%s: injected text %q survived: %q", name, leaked, got)
			}
		}
	}
	if hit, got := stripInjectedInstructions("plain text with <b>markup</b>"); hit || got != "plain text with <b>markup</b>" {
		t.Errorf("unrelated markup was touched: (%v, %q)", hit, got)
	}
}

// Tool arguments are retained wholesale except for two deny rules: the text a
// typing or form-submitting tool sends outward, and any argument whose name
// looks like a credential. The key name goes into a gap; the value never does.
func TestFilterV3DeniesTypedInputAndCredentialArguments(t *testing.T) {
	record := func(name string, input string) string {
		return `{"type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"` + name + `","input":` + input + `}]}}`
	}
	lines := []string{
		record("computer", `{"action":"type","text":"typed-secret-1","coordinate":[10,20]}`),
		record("form_input", `{"ref":"ref_3","value":"typed-secret-2"}`),
		record("mcp__claude-in-chrome__form_input", `{"ref":"ref_4","values":["typed-secret-3"]}`),
		record("mcp__Claude_Browser__computer", `{"action":"key","text":"typed-secret-4"}`),
		record("browser_fill", `{"selector":"#pw","value":"typed-secret-5","nested":{"value":"typed-secret-6"}}`),
		record("Edit", `{"file_path":"/w/a.go","old_string":"x","new_string":"kept-edit-body","text":"kept-edit-text","value":"kept-edit-value"}`),
		record("mcp__github__create_issue", `{"title":"kept-title","github_token":"typed-secret-7","auth":{"CredentialsFile":"typed-secret-8","AuthorizationHeader":"typed-secret-9"},"max_tokens":512}`),
	}
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	for i := 1; i <= 9; i++ {
		if strings.Contains(joined, "typed-secret-"+string(rune('0'+i))) {
			t.Errorf("typed or credential argument %d survived", i)
		}
	}
	for _, kept := range []string{"kept-edit-body", "kept-edit-text", "kept-edit-value", "kept-title", `"ref":"ref_3"`, `"selector":"#pw"`, `"action":"type"`} {
		if !strings.Contains(joined, kept) {
			t.Errorf("ordinary argument %q was dropped", kept)
		}
	}
	records := decodeRecords(t, filtered)
	if len(records) != len(lines) {
		t.Fatalf("retained %d records, want %d", len(records), len(lines))
	}
	edit := child(t, contentBlock(t, child(t, records[5], "message"), "tool_use")["input"])
	for _, key := range []string{"text", "value", "new_string"} {
		if _, kept := edit[key]; !kept {
			t.Errorf("Edit is not a typing tool; its %q argument must stay", key)
		}
	}
	issue := child(t, contentBlock(t, child(t, records[6], "message"), "tool_use")["input"])
	if _, kept := issue["max_tokens"]; kept {
		t.Errorf("max_tokens contains the token fragment and is knowingly dropped: %#v", issue)
	}
	if _, kept := issue["auth"]; kept {
		t.Errorf("an argument object whose members were all denied is pruned with them: %#v", issue)
	}

	var details []string
	for _, gap := range filtered.Gaps {
		if gap.Code == "sensitive_or_hidden_field_omitted" && strings.HasPrefix(gap.Detail, deniedToolArgumentIntro) {
			details = append(details, gap.Detail)
		}
	}
	if len(details) != 1 {
		t.Fatalf("want exactly one denied-argument gap, got %d in %#v", len(details), filtered.Gaps)
	}
	if details[0] != deniedToolArgumentIntro+"AuthorizationHeader, CredentialsFile, github_token, max_tokens, text, value, values" {
		t.Errorf("denied-argument gap detail = %q", details[0])
	}
	if strings.Contains(details[0], "typed-secret") {
		t.Errorf("gap detail leaked a value: %q", details[0])
	}
}

// Cursor carries the tool name in tool_name beside tool_input; the deny list
// must read that spelling too.
func TestFilterV3DeniesTypedInputForCursorToolCalls(t *testing.T) {
	input := `{"type":"tool_call","timestamp":"2026-09-20T10:00:00Z","tool_name":"computer","tool_input":{"action":"type","text":"typed-secret"}}`
	filtered, err := (CursorAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "typed-secret") || !strings.Contains(joined, `"action":"type"`) {
		t.Errorf("cursor typed input handling wrong: %s", joined)
	}
}

// Supplemental evidence has no per-transcript name collector; a denied
// argument there falls back to the content-free gap and is still dropped.
func TestFilterV3DeniedArgumentsInSupplementalEvidenceFallBackToPlainGap(t *testing.T) {
	filtered, gaps, err := FilterSupplementalEvidence([]SupplementalEvidence{{
		//lint:ignore LV1001 deliberately a kind with no declared constant
		Kind: "hook_event", ObservedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), Provenance: "test",
		Payload: map[string]any{"tool_name": "form_input", "tool_input": map[string]any{"ref": "r", "value": "typed-secret"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(filtered)
	if strings.Contains(string(encoded), "typed-secret") {
		t.Fatalf("typed value retained in supplemental evidence: %s", encoded)
	}
	if !hasGap(gaps, "sensitive_or_hidden_field_omitted") {
		t.Errorf("gaps = %#v", gaps)
	}
}

func TestFilterV3RedactsCredentialShapes(t *testing.T) {
	positive := map[string]string{
		"pem":                      "config:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7synthetic\nline2\n-----END RSA PRIVATE KEY-----\ndone",
		"pem openssh unterminated": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAAsynthetic",
		"jwt":                      "header: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzeW50aGV0aWMifQ.c3ludGhldGljLXNpZ25hdHVyZQ end",
		"url userinfo":             "git remote add origin https://x-access-token:ghs_synthetic@github.com/o/r.git",
		"url bare user":            "curl ftp://deploy@files.example.test/x",
		"github classic":           "export GH=ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github fine grained":      "github_pat_11ABCDEFG0123456789_synthetic_value_here",
		"slack bot":                "xoxb-1234567890-synthetic-value",
		"slack user":               "xoxp-1234567890-synthetic-value",
	}
	for name, in := range positive {
		out, hit := redactSensitive(in)
		if !hit || !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s: not redacted: %q", name, out)
		}
		for _, leaked := range []string{"MIIEpAIBAAKCAQEA7synthetic", "line2", "b3BlbnNzaC1rZXktdjEAAAAAsynthetic", "eyJhbGciOiJIUzI1NiJ9", "c3ludGhldGljLXNpZ25hdHVyZQ", "x-access-token:ghs_synthetic", "deploy@", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github_pat_11ABCDEFG", "xoxb-1234567890", "xoxp-1234567890"} {
			if strings.Contains(out, leaked) {
				t.Errorf("%s: %q survived in %q", name, leaked, out)
			}
		}
	}
	// The PEM block's END line is consumed with it; text after it survives.
	if out, _ := redactSensitive(positive["pem"]); !strings.HasPrefix(out, "config:\n[REDACTED]\ndone") {
		t.Errorf("pem redaction shape: %q", out)
	}
	// Only the userinfo of a URL is replaced; scheme and host stay legible.
	if out, _ := redactSensitive(positive["url userinfo"]); !strings.Contains(out, "https://[REDACTED]@github.com/o/r.git") {
		t.Errorf("url userinfo redaction shape: %q", out)
	}
	// The same shapes are redacted when they sit inside a retained tool argument.
	record := `{"type":"assistant","uuid":"a","timestamp":"2026-09-20T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Write","input":{"file_path":"/w/k","content":"` + strings.ReplaceAll(positive["pem"], "\n", `\n`) + `"}}]}}`
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(record))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "MIIEpAIBAAKCAQEA7synthetic") || !hasGap(filtered.Gaps, "sensitive_content_redacted") {
		t.Errorf("pem survived inside a tool argument: %s %#v", joined, filtered.Gaps)
	}
}

// Ordinary code and prose must pass through the structural patterns. The one
// known false-positive class is the pre-existing assignment pattern, which
// this test pins so a change to it is deliberate.
func TestFilterV3DoesNotRedactOrdinaryCode(t *testing.T) {
	negative := []string{
		"tokens := lexer.Tokenize(src)",
		"func parseToken(t Token) error { return nil }",
		"if token.Kind == KindIdent { next() }",
		"see https://example.test/docs/path?x=1#frag and ssh://host/repo",
		"-----BEGIN CERTIFICATE-----\nMIIBsynthetic\n-----END CERTIFICATE-----",
		"eyJ is how a base64 JSON object starts; eyJhbGci.short.x is not a token",
		"ghp_short and xoxb- alone and github_pat_ alone",
		"password_policy = strong",
		"the secretary filed the report",
	}
	for _, in := range negative {
		if out, hit := redactSensitive(in); hit || out != in {
			t.Errorf("ordinary text was redacted: %q -> %q", in, out)
		}
	}
	// Known false positive: the assignment pattern does not know the value is code.
	if out, hit := redactSensitive("token = parse(x)"); !hit || out != "[REDACTED]" {
		t.Errorf("assignment pattern behavior changed: %q (hit=%v)", out, hit)
	}
}

// Two scans of identical input must produce byte-identical records and gaps,
// otherwise a repeated background scan would look like new evidence and mint
// a new source snapshot.
func TestFilterV3OutputIsDeterministicAcrossScans(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter Adapter
		fixture string
	}{
		{"claude", ClaudeAdapter{}, "claude-tool-evidence.jsonl"},
		{"codex", CodexAdapter{}, "codex-tool-and-usage.jsonl"},
		{"cursor", CursorAdapter{}, "cursor-turn.jsonl"},
	} {
		first, err := tc.adapter.FilterJSONL(bytes.NewReader(fixture(t, tc.fixture)))
		if err != nil {
			t.Fatal(err)
		}
		second, err := tc.adapter.FilterJSONL(bytes.NewReader(fixture(t, tc.fixture)))
		if err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(second)
		if !bytes.Equal(a, b) {
			t.Errorf("%s: repeated scan differs:\n%s\n%s", tc.name, a, b)
		}
	}
}
