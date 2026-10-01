package archive

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeRecordsOfType returns the retained records of one type.
func claudeRecordsOfType(records []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, record := range records {
		if record["type"] == kind {
			out = append(out, record)
		}
	}
	return out
}

func filterClaudeLines(t *testing.T, lines ...string) (FilteredTranscript, []map[string]any) {
	t.Helper()
	return filteredRecords(t, ClaudeAdapter{}, strings.Join(lines, "\n")+"\n")
}

// A custom-title record is kept with its title, its session, and nothing else;
// the title is redacted as prompt text is.
func TestFilterV13KeepsACustomTitleAndRedactsIt(t *testing.T) {
	t.Parallel()
	filtered, records := filteredRecords(t, ClaudeAdapter{}, string(fixture(t, "claude-session-name.jsonl")))
	titles := claudeRecordsOfType(records, "custom-title")
	if len(titles) != 1 {
		t.Fatalf("retained %d custom-title records, want 1: %#v", len(titles), records)
	}
	if got := strings.Join(sortedKeys(titles[0]), " "); got != "customTitle sessionId type" {
		t.Fatalf("custom-title keys = %q", got)
	}
	if titles[0]["customTitle"] != "Rename the widget parser password=[REDACTED]" || titles[0]["sessionId"] != "native-named" {
		t.Fatalf("custom-title = %#v", titles[0])
	}
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(encoded, "hunter2") {
		t.Fatalf("the secret in the title was retained: %s", encoded)
	}
	if !hasGap(filtered.Gaps, "sensitive_content_redacted") {
		t.Errorf("the redaction is not recorded: %#v", filtered.Gaps)
	}
}

// Keys beyond the ones filter 13 keeps are dropped and reported by name, and a
// title that is empty, not text, or only whitespace drops the record with a gap.
func TestFilterV13CustomTitleKeepsOnlyItsTitle(t *testing.T) {
	t.Parallel()
	filtered, records := filterClaudeLines(t,
		`{"type":"custom-title","customTitle":"Named","sessionId":"s1","cwd":"/work/synthetic","uuid":"u1","nested":{"customTitle":"SYNTHETIC-NESTED"}}`,
		`{"type":"custom-title","customTitle":"","sessionId":"s1"}`,
		`{"type":"custom-title","customTitle":"   ","sessionId":"s1"}`,
		`{"type":"custom-title","customTitle":{"text":"SYNTHETIC-OBJECT"},"sessionId":"s1"}`,
		`{"type":"custom-title","sessionId":"s1"}`,
		`{"type":"custom-title","customTitle":"Typed ids","sessionId":{"id":"SYNTHETIC-ID"},"timestamp":["SYNTHETIC-TIME"]}`,
	)
	titles := claudeRecordsOfType(records, "custom-title")
	if len(titles) != 2 || titles[0]["customTitle"] != "Named" || titles[1]["customTitle"] != "Typed ids" {
		t.Fatalf("custom-title records = %#v", titles)
	}
	if got := strings.Join(sortedKeys(titles[0]), " "); got != "customTitle sessionId type" {
		t.Fatalf("custom-title keys = %q", got)
	}
	if got := strings.Join(sortedKeys(titles[1]), " "); got != "customTitle type" {
		t.Fatalf("custom-title keys = %q", got)
	}
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, leaked := range []string{"SYNTHETIC", "/work/synthetic", "u1"} {
		if strings.Contains(encoded, leaked) {
			t.Errorf("retained %q: %s", leaked, encoded)
		}
	}
	omitted := gapDetail(filtered.Gaps, "unknown_field_omitted")
	for _, name := range []string{"cwd", "nested", "uuid", "sessionId", "timestamp"} {
		if !strings.Contains(omitted, name) {
			t.Errorf("omitted key %q is not reported: %q", name, omitted)
		}
	}
	if !hasGapDetail(filtered.Gaps, "unsupported_value_omitted", "record omitted") {
		t.Errorf("the dropped records are not recorded: %#v", filtered.Gaps)
	}
}

// Claude Code appends a custom-title record for each name a session is given.
// Every one is kept, so a renamed session keeps its earlier names too, as
// docs/security/privacy.md says.
func TestFilterV13KeepsEveryNameASessionHad(t *testing.T) {
	t.Parallel()
	_, records := filterClaudeLines(t,
		`{"type":"custom-title","customTitle":"First name","sessionId":"s1"}`,
		`{"type":"custom-title","customTitle":"Second name","sessionId":"s1"}`,
	)
	titles := claudeRecordsOfType(records, "custom-title")
	if len(titles) != 2 || titles[0]["customTitle"] != "First name" || titles[1]["customTitle"] != "Second name" {
		t.Fatalf("custom-title records = %#v", titles)
	}
}

// A pr-link is kept with the pull request's number as an integer, its
// repository, and its URL when that is the GitHub URL of those two, however
// the number was written.
func TestFilterV13KeepsAPRLink(t *testing.T) {
	t.Parallel()
	_, records := filteredRecords(t, ClaudeAdapter{}, string(fixture(t, "claude-session-name.jsonl")))
	links := claudeRecordsOfType(records, "pr-link")
	if len(links) != 2 {
		t.Fatalf("retained %d pr-link records, want 2: %#v", len(links), links)
	}
	wantKeys := "prNumber prRepository prUrl sessionId timestamp type"
	first := links[0]
	if got := strings.Join(sortedKeys(first), " "); got != wantKeys {
		t.Fatalf("pr-link keys = %q, want %q", got, wantKeys)
	}
	if first["prNumber"] != float64(213) || first["prRepository"] != "example-org/widget-tools" ||
		first["prUrl"] != "https://github.com/example-org/widget-tools/pull/213" ||
		first["sessionId"] != "native-named" || first["timestamp"] != "2026-09-30T10:05:00Z" {
		t.Fatalf("pr-link = %#v", first)
	}
	// Written as a number, the same shape comes out.
	if links[1]["prNumber"] != float64(214) {
		t.Fatalf("pr-link with a numeric prNumber = %#v", links[1])
	}
}

// Both forms of the number produce the same encoded record.
func TestFilterV13PRNumberIsTypeStable(t *testing.T) {
	t.Parallel()
	forms := []string{`"213"`, `213`, `213.0`, `"0213"`}
	var want string
	for _, form := range forms {
		filtered, _ := filterClaudeLines(t, `{"type":"pr-link","prNumber":`+form+`,"prRepository":"example-org/widget-tools","sessionId":"s1"}`)
		if len(filtered.Records) != 1 {
			t.Fatalf("prNumber %s: retained %d records", form, len(filtered.Records))
		}
		got := string(filtered.Records[0])
		if want == "" {
			want = got
		}
		if got != want {
			t.Errorf("prNumber %s encodes as %s, want %s", form, got, want)
		}
	}
	if !strings.Contains(want, `"prNumber":213`) {
		t.Errorf("record = %s", want)
	}
}

// A prUrl that is not the GitHub URL of the repository and number is dropped
// and the rest of the record is kept; the dropped key is named in a gap.
func TestFilterV13DropsAPRURLThatIsNotTheRebuiltOne(t *testing.T) {
	t.Parallel()
	for name, link := range map[string]string{
		"a query string":     "https://github.com/example-org/widget-tools/pull/213?token=SYNTHETIC-SECRET",
		"another host":       "https://ghe.example.com/example-org/widget-tools/pull/213",
		"another number":     "https://github.com/example-org/widget-tools/pull/999",
		"another repository": "https://github.com/example-org/other/pull/213",
		"http":               "http://github.com/example-org/widget-tools/pull/213",
		"a fragment":         "https://github.com/example-org/widget-tools/pull/213#discussion",
		"a trailing slash":   "https://github.com/example-org/widget-tools/pull/213/",
		"empty":              "",
	} {
		filtered, records := filterClaudeLines(t, `{"type":"pr-link","prNumber":"213","prRepository":"example-org/widget-tools","prUrl":"`+link+`","sessionId":"s1"}`)
		links := claudeRecordsOfType(records, "pr-link")
		if len(links) != 1 {
			t.Fatalf("%s: retained %d pr-link records, want 1", name, len(links))
		}
		if _, kept := links[0]["prUrl"]; kept {
			t.Errorf("%s: prUrl retained: %#v", name, links[0])
		}
		if links[0]["prNumber"] != float64(213) || links[0]["prRepository"] != "example-org/widget-tools" {
			t.Errorf("%s: pr-link = %#v", name, links[0])
		}
		if !strings.Contains(gapDetail(filtered.Gaps, "unknown_field_omitted"), "prUrl") {
			t.Errorf("%s: the dropped prUrl is not reported: %#v", name, filtered.Gaps)
		}
		if strings.Contains(string(bytes.Join(filtered.Records, nil)), "SYNTHETIC-SECRET") {
			t.Errorf("%s: the URL's secret was retained", name)
		}
	}
	// A prUrl that is not a string is dropped the same way.
	_, records := filterClaudeLines(t, `{"type":"pr-link","prNumber":"213","prRepository":"example-org/widget-tools","prUrl":{"href":"x"},"sessionId":"s1"}`)
	if links := claudeRecordsOfType(records, "pr-link"); len(links) != 1 || links[0]["prUrl"] != nil {
		t.Errorf("pr-link with an object prUrl = %#v", links)
	}
}

// A pr-link whose repository or number is missing or out of shape is dropped
// whole, with a gap that carries no content.
func TestFilterV13DropsAMalformedPRLinkWithAGap(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"a repository with a space":  `"prNumber":"213","prRepository":"example-org/widget tools"`,
		"a repository with no owner": `"prNumber":"213","prRepository":"widget-tools"`,
		"a repository with a path":   `"prNumber":"213","prRepository":"example-org/widget/tools"`,
		"a repository with a scheme": `"prNumber":"213","prRepository":"https://github.com/example-org/widget-tools"`,
		"a repository as an object":  `"prNumber":"213","prRepository":{"name":"x"}`,
		"no repository":              `"prNumber":"213"`,
		"no number":                  `"prRepository":"example-org/widget-tools"`,
		"a zero number":              `"prNumber":"0","prRepository":"example-org/widget-tools"`,
		"a negative number":          `"prNumber":-3,"prRepository":"example-org/widget-tools"`,
		"a fractional number":        `"prNumber":2.5,"prRepository":"example-org/widget-tools"`,
		"a number with a sign":       `"prNumber":"+5","prRepository":"example-org/widget-tools"`,
		"a number with text":         `"prNumber":"12abc","prRepository":"example-org/widget-tools"`,
		"a number out of range":      `"prNumber":"99999999999","prRepository":"example-org/widget-tools"`,
		"a number as a bool":         `"prNumber":true,"prRepository":"example-org/widget-tools"`,
		"an empty number":            `"prNumber":"","prRepository":"example-org/widget-tools"`,
	} {
		filtered, records := filterClaudeLines(t,
			`{"type":"user","uuid":"a","sessionId":"s1","timestamp":"2026-09-30T10:00:00Z","message":{"role":"user","content":"hello"}}`,
			`{"type":"pr-link","sessionId":"s1",`+body+`,"prUrl":"https://github.com/example-org/widget-tools/pull/213"}`)
		if links := claudeRecordsOfType(records, "pr-link"); len(links) != 0 {
			t.Errorf("%s: the record was retained: %#v", name, links)
		}
		if !hasGapDetail(filtered.Gaps, "unsupported_value_omitted", "record omitted") {
			t.Errorf("%s: the drop is not recorded: %#v", name, filtered.Gaps)
		}
		if hasGap(filtered.Gaps, "unknown_record_type") {
			t.Errorf("%s: pr-link is no longer an unknown record type: %#v", name, filtered.Gaps)
		}
		gaps, _ := json.Marshal(filtered.Gaps)
		if strings.Contains(string(gaps), "example-org") {
			t.Errorf("%s: a gap carries a value: %s", name, gaps)
		}
	}
}

// A pr-link whose repository the value rules would rewrite is dropped, not
// kept as a link to a repository it does not name.
func TestFilterV13DropsAPRLinkWhoseRepositoryIsRedacted(t *testing.T) {
	t.Parallel()
	_, records := filterClaudeLines(t, `{"type":"pr-link","prNumber":"1","prRepository":"AKIAIOSFODNN7EXAMPLE/AKIAIOSFODNN7EXAMPLE","sessionId":"s1"}`)
	if links := claudeRecordsOfType(records, "pr-link"); len(links) != 0 {
		t.Errorf("retained %#v", links)
	}
}

// The names are admitted only on their own record types, and agent-name and
// last-prompt are still dropped.
func TestFilterV13AdmitsTheKeysOnlyOnTheirRecords(t *testing.T) {
	t.Parallel()
	filtered, records := filterClaudeLines(t,
		`{"type":"user","uuid":"a","sessionId":"s1","customTitle":"SYNTHETIC-TITLE","prNumber":"5","prRepository":"example-org/widget-tools","prUrl":"https://example.com/x","timestamp":"2026-09-30T10:00:00Z","message":{"role":"user","content":"hello"}}`,
		`{"type":"custom-title","customTitle":"Named","prNumber":"5","prRepository":"example-org/widget-tools","sessionId":"s1"}`,
		`{"type":"pr-link","prNumber":"5","prRepository":"example-org/widget-tools","customTitle":"SYNTHETIC-TITLE-TWO","sessionId":"s1"}`,
		`{"type":"agent-name","agentName":"SYNTHETIC-AGENT-NAME","sessionId":"s1"}`,
		`{"type":"last-prompt","lastPrompt":"SYNTHETIC-LAST-PROMPT","sessionId":"s1"}`,
	)
	encoded := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, leaked := range []string{"SYNTHETIC", "https://example.com"} {
		if strings.Contains(encoded, leaked) {
			t.Errorf("retained %q: %s", leaked, encoded)
		}
	}
	if got := strings.Join(sortedKeys(records[1]), " "); got != "customTitle sessionId type" {
		t.Errorf("custom-title keys = %q", got)
	}
	if got := strings.Join(sortedKeys(records[2]), " "); got != "prNumber prRepository sessionId type" {
		t.Errorf("pr-link keys = %q", got)
	}
	if len(records) != 3 || !hasGap(filtered.Gaps, "unknown_record_type") {
		t.Errorf("agent-name and last-prompt must stay dropped: %d records, gaps %#v", len(records), filtered.Gaps)
	}
}

// Only Claude Code writes these records. The same shapes in another harness's
// JSONL are not recognized.
func TestFilterV13CustomTitleIsAdmittedOnlyFromClaude(t *testing.T) {
	t.Parallel()
	for _, adapter := range []Adapter{CodexAdapter{}, CursorAdapter{}} {
		filtered, err := adapter.FilterJSONL(strings.NewReader(`{"type":"custom-title","customTitle":"SYNTHETIC-TITLE","sessionId":"s1"}` + "\n"))
		if err == nil && len(filtered.Records) != 0 {
			t.Errorf("%s retained %s", adapter.Name(), bytes.Join(filtered.Records, []byte("\n")))
		}
	}
}

func chatWithName(t *testing.T, name string) CursorComposer {
	t.Helper()
	composer := loadComposerFixture(t, "chat.json")
	var value map[string]any
	if err := json.Unmarshal(composer.Composer, &value); err != nil {
		t.Fatal(err)
	}
	if name == "\x00absent" {
		delete(value, "name")
	} else {
		value["name"] = json.RawMessage(name)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	composer.Composer = encoded
	return composer
}

// The chat's name is kept on the session record, redacted as text, and is no
// longer reported as an omitted key.
func TestFilterV13CursorChatNameIsKeptAndRedacted(t *testing.T) {
	t.Parallel()
	filtered, records := filterComposerFixture(t)
	if records[0]["type"] != "session" || records[0]["name"] != "Fix the widget test password=[REDACTED]" {
		t.Fatalf("session record = %#v", records[0])
	}
	if got := strings.Join(sortedKeys(records[0]), " "); got != "name session_id timestamp type" {
		t.Fatalf("session record keys = %q", got)
	}
	if strings.Contains(string(bytes.Join(filtered.Records, nil)), "hunter2") {
		t.Fatal("the secret in the name was retained")
	}
	if strings.Contains(gapDetail(filtered.Gaps, "unknown_field_omitted"), "chat.name") {
		t.Errorf("chat.name is reported as omitted: %q", gapDetail(filtered.Gaps, "unknown_field_omitted"))
	}
}

// A chat with no name, an empty name, or a name that is not text has no name
// on its session record; only a name that is not text is reported.
func TestFilterV13CursorChatWithoutATextName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		raw      string
		reported bool
	}{
		"absent":     {"\x00absent", false},
		"null":       {`null`, false},
		"empty":      {`""`, false},
		"whitespace": {`"  \n "`, false},
		"a number":   {`12`, true},
		"an object":  {`{"text":"SYNTHETIC-NAME"}`, true},
	} {
		filtered, err := (CursorAdapter{}).FilterComposer(chatWithName(t, tc.raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var session map[string]any
		if err := json.Unmarshal(filtered.Records[0], &session); err != nil {
			t.Fatal(err)
		}
		if _, has := session["name"]; has {
			t.Errorf("%s: session record has a name: %#v", name, session)
		}
		if got := strings.Contains(gapDetail(filtered.Gaps, "unknown_field_omitted"), "chat.name"); got != tc.reported {
			t.Errorf("%s: chat.name reported as omitted = %v, want %v", name, got, tc.reported)
		}
		if strings.Contains(string(bytes.Join(filtered.Records, nil)), "SYNTHETIC-NAME") {
			t.Errorf("%s: retained the object's text", name)
		}
	}
}

// A name longer than the string cap is cut like any retained text.
func TestFilterV13CursorChatNameIsCapped(t *testing.T) {
	t.Parallel()
	filtered, err := (CursorAdapter{}).FilterComposer(chatWithName(t, `"`+strings.Repeat("n", maxTextBytes+10)+`"`))
	if err != nil {
		t.Fatal(err)
	}
	var session map[string]any
	if err := json.Unmarshal(filtered.Records[0], &session); err != nil {
		t.Fatal(err)
	}
	if name, _ := session["name"].(string); len(name) != maxTextBytes {
		t.Fatalf("name length = %d, want %d", len(name), maxTextBytes)
	}
	if !hasGap(filtered.Gaps, "content_truncated") {
		t.Errorf("gaps = %#v", filtered.Gaps)
	}
}

// Parser 0.17.0 reads the new records into the name and pull requests and
// nothing else: metadata built from a transcript with them equals metadata
// built from the same transcript without them, apart from those two fields and
// the gaps the filter recorded for them.
func TestFilterV13NewRecordsChangeOnlyNameAndPullRequests(t *testing.T) {
	t.Parallel()
	raw := string(fixture(t, "claude-session-name.jsonl"))
	var without []string
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		if !strings.Contains(line, `"pr-link"`) && !strings.Contains(line, `"custom-title"`) {
			without = append(without, line)
		}
	}
	derive := func(jsonl string) Metadata {
		filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(jsonl))
		if err != nil {
			t.Fatal(err)
		}
		reg := registration()
		reg.Harness = Harness{Name: "claude"}
		now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
		bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, now, nil)
		if err != nil {
			t.Fatal(err)
		}
		ref := SourceReference{Key: "sessions/claude/a/source.jsonl.gz", SHA256: strings.Repeat("a", 64)}
		metadata, err := BuildMetadata(bundle, "machine", now, now, ref, ParserInfo{})
		if err != nil {
			t.Fatal(err)
		}
		metadata.CaptureGaps = nil
		return metadata
	}
	with, bare := derive(raw), derive(strings.Join(without, "\n")+"\n")
	if with.Name != "Rename the widget parser password=[REDACTED]" || len(with.PullRequests) != 2 {
		t.Fatalf("labels = name %q, pull requests %#v", with.Name, with.PullRequests)
	}
	with.Name, with.PullRequests = "", nil
	encode := func(m Metadata) string {
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	if encode(with) != encode(bare) {
		t.Fatalf("metadata changed:\nwith:    %s\nwithout: %s", encode(with), encode(bare))
	}
}

// The filter changelog has a section for the current filter version, and the
// one for filter 13 names every key it newly keeps and the records that stay
// dropped; the one for filter 14 names the record it writes and what it drops.
func TestFilterChangelogDescribesTheCurrentVersion(t *testing.T) {
	t.Parallel()
	changelog, err := os.ReadFile(filepath.Join("..", "..", "dev", "specs", "privacy-filter-changelog.md"))
	if err != nil {
		t.Fatal(err)
	}
	section := func(version string) string {
		_, rest, found := strings.Cut(string(changelog), "\n## Source filter version "+version+"\n")
		if !found {
			t.Fatalf("the filter changelog has no section for version %s", version)
		}
		body, _, _ := strings.Cut(rest, "\n## ")
		return body
	}
	section(FilterVersion)
	thirteen := section("13")
	for _, want := range []string{"custom-title", "customTitle", "pr-link", "prNumber", "prRepository", "prUrl", "sessionId", "timestamp", "chat.name", "agent-name", "last-prompt", "0.13.0"} {
		if !strings.Contains(thirteen, want) {
			t.Errorf("the version 13 section does not mention %q", want)
		}
	}
	fourteen := section("14")
	for _, want := range []string{"subagent-meta", ".meta.json", "description", "worktreePath", "agentType", "512 bytes", "0.14.0", "0.18.0"} {
		if !strings.Contains(fourteen, want) {
			t.Errorf("the version 14 section does not mention %q", want)
		}
	}
}

// A prUrl the value rules changed or dropped is left out and reported by name,
// as one claudeLabelRecord drops is; the record itself is kept.
func TestFilterV13PRURLChangedBySanitizingIsReported(t *testing.T) {
	t.Parallel()
	label := map[string]any{"type": "pr-link", "prNumber": float64(5), "prRepository": "example-org/widget-tools", "prUrl": "https://github.com/example-org/widget-tools/pull/5"}
	for name, safeURL := range map[string]any{"changed": "https://github.com/[REDACTED]/pull/5", "dropped": nil} {
		safe := map[string]any{"type": "pr-link", "prNumber": float64(5), "prRepository": "example-org/widget-tools"}
		if safeURL != nil {
			safe["prUrl"] = safeURL
		}
		var omitted []string
		if !claudeLabelSurvived(label, safe, func(key string) { omitted = append(omitted, key) }) {
			t.Fatalf("%s: the record was dropped", name)
		}
		if _, kept := safe["prUrl"]; kept || strings.Join(omitted, " ") != "prUrl" {
			t.Errorf("%s: record %#v, omitted %q", name, safe, omitted)
		}
	}
}

// Only a Cursor chat's session record may differ in its name and still be the
// same evidence; a message, or another format's record, may not.
func TestSameNativeRecordIgnoresOnlyACursorChatName(t *testing.T) {
	t.Parallel()
	session := map[string]any{"type": "session", "session_id": "c1", "timestamp": "2026-09-30T10:00:00Z"}
	named := map[string]any{"type": "session", "session_id": "c1", "timestamp": "2026-09-30T10:00:00Z", "name": "Fix the widget test"}
	renamed := map[string]any{"type": "session", "session_id": "c1", "timestamp": "2026-09-30T10:00:00Z", "name": "Fix the widget parser"}
	if !SameNativeRecord(cursorComposerFormat, session, named) || !SameNativeRecord(cursorComposerFormat, named, renamed) || !SameNativeRecord(cursorComposerFormat, named, session) {
		t.Error("naming a Cursor chat changed its session record's evidence")
	}
	other := map[string]any{"type": "session", "session_id": "c2", "name": "Fix the widget test"}
	if SameNativeRecord(cursorComposerFormat, named, other) {
		t.Error("a session record with another chat's ID is the same")
	}
	if SameNativeRecord("claude-jsonl", session, named) {
		t.Error("a name on another format's record is ignored")
	}
	message := map[string]any{"role": "user", "id": "b1", "name": "a"}
	if SameNativeRecord(cursorComposerFormat, message, map[string]any{"role": "user", "id": "b1", "name": "b"}) {
		t.Error("a name on a Cursor message is ignored")
	}
}
