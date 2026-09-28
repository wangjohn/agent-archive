package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestUniqueShortIDsLengthensCollisions(t *testing.T) {
	t.Parallel()
	ids := []string{
		"abcdef00111111111111111111111111",
		"abcdef00222222222222222222222222",
		"zzzzzzzz333333333333333333333333",
	}
	shorts := uniqueShortIDs(ids)
	if len(shorts[0]) < 9 || shorts[0] == shorts[1] {
		t.Fatalf("colliding prefixes not lengthened: %v", shorts)
	}
	if shorts[0][:8] != "abcdef00" || shorts[2][:8] != "zzzzzzzz" {
		t.Fatalf("unexpected short ids: %v", shorts)
	}
	// Every row in one listing shares the same prefix length.
	if len(shorts[0]) != len(shorts[1]) || len(shorts[1]) != len(shorts[2]) {
		t.Fatalf("prefix lengths should match: %v", shorts)
	}
}

func TestFormatSessionRowsDefaultAndVerbose(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	m := archive.Metadata{
		SessionID:  "abcdef0123456789abcdef0123456789",
		Title:      "Fix flaky OAuth callback tests",
		ProjectName: "agent-archive",
		CapturedAt: now.Add(-2 * time.Hour),
		Harness:    archive.Harness{Name: "claude"},
		ProjectID:  "proj",
		Parser:     archive.ParserInfo{Status: archive.ParserStatusPartial},
		Models:     []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "claude-opus-5"}}},
		SkillsUsed: []archive.SkillUse{{Name: "code-review"}},
		Origin:     archive.SessionOriginImport,
	}
	rows := formatSessionRows([]archive.Metadata{m}, listFormatOptions{
		Now: now, Projects: map[string]string{"proj": "ignored-local"},
	})
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	r := rows[0]
	if r.Title != "Fix flaky OAuth callback tests" || r.ShortID != "abcdef01" || r.When != "2 hours ago" || r.Project != "agent-archive" {
		t.Fatalf("row=%+v", r)
	}
	if r.Model != "claude-opus-5" || r.SkillHint != " · code-review" || r.Origin != "imported" {
		t.Fatalf("row=%+v", r)
	}
}

func TestProjectLabelsUsesBasename(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "my-app")
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{
		{ProjectID: "a", Root: root, Included: true},
		{ProjectID: "b", Root: filepath.Join(t.TempDir(), "other"), Included: false},
	}}}
	labels := projectLabels(cfg)
	if labels["a"] != "my-app" {
		t.Fatalf("labels=%v", labels)
	}
	if _, ok := labels["b"]; ok {
		t.Fatalf("excluded project should be omitted: %v", labels)
	}
}

func TestMatchBrowseRow(t *testing.T) {
	t.Parallel()
	rows := []listRow{
		{Index: 1, SessionID: "aaaabbbbccccddddeeeeffff00001111", ShortID: "aaaabbbb", HarnessKey: "claude"},
		{Index: 2, SessionID: "1234567890abcdef1234567890abcdef", ShortID: "12345678", HarnessKey: "codex"},
	}
	if got, ok := matchBrowseRow("2", rows); !ok || got.SessionID != rows[1].SessionID {
		t.Fatalf("number match: ok=%v got=%+v", ok, got)
	}
	if got, ok := matchBrowseRow("aaaabbbb", rows); !ok || got.Index != 1 {
		t.Fatalf("short id: ok=%v got=%+v", ok, got)
	}
	// An all-decimal short id must not be treated as a row index.
	if got, ok := matchBrowseRow("12345678", rows); !ok || got.SessionID != rows[1].SessionID {
		t.Fatalf("decimal short id: ok=%v got=%+v", ok, got)
	}
	if _, ok := matchBrowseRow("9", rows); ok {
		t.Fatal("out of range should miss")
	}
}
